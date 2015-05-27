package cluster

import (
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"sync"

	"github.com/influxdb/influxdb/tsdb"
)

var (
	// ErrBindAddressRequired is returned when starting the Server
	// without providing a bind address
	ErrBindAddressRequired = errors.New("bind address required")

	// ErrServerClosed return when closing an already closed graphite server.
	ErrServerClosed = errors.New("server already closed")
)

type writer interface {
	WriteShard(shardID uint64, points []tsdb.Point) error
}

// Server processes data received over raw TCP connections.
type Server struct {
	writer   writer
	listener *net.Listener

	wg sync.WaitGroup

	Logger *log.Logger

	shutdown chan struct{}

	mu sync.RWMutex
	// the actual addr the server opens on
	addr net.Addr
	// used to initially spin up the server, could be a zero port
	laddr string
}

// NewServer returns a new instance of a Server.
func NewServer(w writer, laddr string) *Server {
	return &Server{
		writer:   w,
		laddr:    laddr,
		Logger:   log.New(os.Stderr, "[tcp] ", log.LstdFlags),
		shutdown: make(chan struct{}),
	}
}

// Open instructs the Server to start processing connections
func (s *Server) Open() error {
	if s.laddr == "" { // Make sure we have an laddr
		return ErrBindAddressRequired
	}

	ln, err := net.Listen("tcp", s.laddr)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.listener = &ln
	s.addr = ln.Addr()
	s.mu.Unlock()

	s.Logger.Println("listening on TCP connection", ln.Addr().String())
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		for {
			// Are we shutting down? If so, exit
			select {
			case <-s.shutdown:
				return
			default:
			}

			conn, err := ln.Accept()
			if opErr, ok := err.(*net.OpError); ok && !opErr.Temporary() {
				s.Logger.Println("error temporarily accepting TCP connection", err.Error())
				continue
			}
			if err != nil {
				s.Logger.Println("TCP listener closed")
				return
			}

			s.wg.Add(1)
			go s.handleConnection(conn)
		}
	}()

	return nil
}

// Close will close the listener
func (s *Server) Close() error {
	// Stop accepting client connections
	if s.listener != nil {
		err := (*s.listener).Close()
		if err != nil {
			return err
		}
	} else {
		return ErrServerClosed
	}
	// Shut down all handlers
	close(s.shutdown)
	s.wg.Wait()
	s.listener = nil

	return nil
}

// handleConnection services an individual TCP connection.
func (s *Server) handleConnection(conn net.Conn) {

	// Start our reader up in a go routine so we don't block checking our close channel
	go func() {
		for {
			var messageType byte

			err := binary.Read(conn, binary.LittleEndian, &messageType)
			if err != nil {
				s.Logger.Printf("unable to read message type %s", err)
				return
			}
			s.processMessage(messageType, conn)

			select {
			case <-s.shutdown:
				// Are we shutting down? If so, exit
				return
			default:
			}
		}
	}()

	for {
		select {
		case <-s.shutdown:
			// Are we shutting down? If so, exit
			conn.Close()
			s.wg.Done()
			return
		default:
		}
	}
}

func (s *Server) processMessage(messageType byte, conn net.Conn) {
	switch messageType {
	case writeShardRequestMessage:
		err := s.writeShardRequest(conn)
		s.writeShardResponse(conn, err)
		return
	}
	return
}

func (s *Server) writeShardRequest(conn net.Conn) error {
	var size int64
	if err := binary.Read(conn, binary.LittleEndian, &size); err != nil {
		return err
	}

	message := make([]byte, size)

	reader := io.LimitReader(conn, size)
	if _, err := reader.Read(message); err != nil {
		return err
	}

	var wsr WriteShardRequest
	if err := wsr.UnmarshalBinary(message); err != nil {
		return err
	}
	return s.writer.WriteShard(wsr.ShardID(), wsr.Points())
}

func (s *Server) writeShardResponse(conn net.Conn, e error) {
	var mt byte = writeShardResponseMessage
	if err := binary.Write(conn, binary.LittleEndian, &mt); err != nil {
		s.Logger.Printf("error writing shard response message type: %s", err)
		return
	}

	var wsr WriteShardResponse
	if e != nil {
		wsr.SetCode(1)
		wsr.SetMessage(e.Error())
	} else {
		wsr.SetCode(0)
	}

	b, err := wsr.MarshalBinary()
	if err != nil {
		s.Logger.Printf("error marshalling shard response: %s", err)
		return
	}

	size := int64(len(b))

	if err := binary.Write(conn, binary.LittleEndian, &size); err != nil {
		s.Logger.Printf("error writing shard response length: %s", err)
		return
	}

	if _, err := conn.Write(b); err != nil {
		s.Logger.Printf("error writing shard response: %s", err)
		return
	}
}

func (s *Server) Addr() net.Addr {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.addr

}