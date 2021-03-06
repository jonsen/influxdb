package graphite

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/influxdb/influxdb/tsdb"
)

var defaultTemplate *template

func init() {
	var err error
	defaultTemplate, err = NewTemplate("measurement*", nil, DefaultSeparator)
	if err != nil {
		panic(err)
	}
}

// Parser encapsulates a Graphite Parser.
type Parser struct {
	matcher *matcher
	tags    tsdb.Tags
}

// Options are configurable values that can be provided to a Parser
type Options struct {
	Separator   string
	Templates   []string
	DefaultTags tsdb.Tags
}

// NewParserWithOptions returns a graphite parser using the given options
func NewParserWithOptions(options Options) (*Parser, error) {

	matcher := newMatcher()
	matcher.AddDefaultTemplate(defaultTemplate)

	for _, pattern := range options.Templates {

		template := pattern
		filter := ""
		// Format is [filter] <template> [tag1=value1,tag2=value2]
		parts := strings.Fields(pattern)
		if len(parts) >= 2 {
			filter = parts[0]
			template = parts[1]
		}

		// Parse out the default tags specific to this template
		tags := tsdb.Tags{}
		if strings.Contains(parts[len(parts)-1], "=") {
			tagStrs := strings.Split(parts[len(parts)-1], ",")
			for _, kv := range tagStrs {
				parts := strings.Split(kv, "=")
				tags[parts[0]] = parts[1]
			}
		}

		tmpl, err := NewTemplate(template, tags, options.Separator)
		if err != nil {
			return nil, err
		}
		matcher.Add(filter, tmpl)
	}
	return &Parser{matcher: matcher, tags: options.DefaultTags}, nil
}

// NewParser returns a GraphiteParser instance.
func NewParser(templates []string, defaultTags tsdb.Tags) (*Parser, error) {
	return NewParserWithOptions(
		Options{
			Templates:   templates,
			DefaultTags: defaultTags,
			Separator:   DefaultSeparator,
		})
}

// Parse performs Graphite parsing of a single line.
func (p *Parser) Parse(line string) (tsdb.Point, error) {
	// Break into 3 fields (name, value, timestamp).
	fields := strings.Fields(line)
	if len(fields) != 2 && len(fields) != 3 {
		return nil, fmt.Errorf("received %q which doesn't have required fields", line)
	}

	// decode the name and tags
	matcher := p.matcher.Match(fields[0])
	measurement, tags := matcher.Apply(fields[0])

	// Could not extract measurement, use the raw value
	if measurement == "" {
		measurement = fields[0]
	}

	// Parse value.
	v, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return nil, fmt.Errorf(`field "%s" value: %s`, fields[0], err)
	}

	fieldValues := map[string]interface{}{"value": v}

	// If no 3rd field, use now as timestamp
	timestamp := time.Now().UTC()

	if len(fields) == 3 {
		// Parse timestamp.
		unixTime, err := strconv.ParseFloat(fields[2], 64)
		if err != nil {
			return nil, fmt.Errorf(`field "%s" time: %s`, fields[0], err)
		}

		// -1 is a special value that gets converted to current UTC time
		// See https://github.com/graphite-project/carbon/issues/54
		if unixTime != float64(-1) {
			// Check if we have fractional seconds
			timestamp = time.Unix(int64(unixTime), int64((unixTime-math.Floor(unixTime))*float64(time.Second)))
		}
	}

	// Set the default tags on the point if they are not already set
	for k, v := range p.tags {
		if _, ok := tags[k]; !ok {
			tags[k] = v
		}
	}
	point := tsdb.NewPoint(measurement, tags, fieldValues, timestamp)

	return point, nil
}

// template represents a pattern and tags to map a graphite metric string to a influxdb Point
type template struct {
	tags              []string
	defaultTags       tsdb.Tags
	greedyMeasurement bool
	separator         string
}

func NewTemplate(pattern string, defaultTags tsdb.Tags, separator string) (*template, error) {
	tags := strings.Split(pattern, ".")
	hasMeasurement := false
	template := &template{tags: tags, defaultTags: defaultTags, separator: separator}

	for _, tag := range tags {
		if strings.HasPrefix(tag, "measurement") {
			hasMeasurement = true
		}
		if tag == "measurement*" {
			template.greedyMeasurement = true
		}
	}

	if !hasMeasurement {
		return nil, fmt.Errorf("no measurement specified for template. %q", pattern)
	}

	return template, nil
}

// Apply extracts the template fields form the given line and returns the measurement
// name and tags
func (t *template) Apply(line string) (string, map[string]string) {
	fields := strings.Split(line, ".")
	var (
		measurement []string
		tags        = make(map[string]string)
	)

	// Set any default tags
	for k, v := range t.defaultTags {
		tags[k] = v
	}

	for i, tag := range t.tags {
		if i >= len(fields) {
			continue
		}

		if tag == "measurement" {
			measurement = append(measurement, fields[i])
		} else if tag == "measurement*" {
			measurement = append(measurement, fields[i:]...)
			break
		} else if tag != "" {
			tags[tag] = fields[i]
		}
	}

	return strings.Join(measurement, t.separator), tags
}

// matcher determines which template should be applied to a given metric
// based on a filter tree.
type matcher struct {
	root            *node
	defaultTemplate *template
}

func newMatcher() *matcher {
	return &matcher{
		root: &node{},
	}
}

// Add inserts the template in the filter tree based the given filter
func (m *matcher) Add(filter string, template *template) {
	if filter == "" {
		m.AddDefaultTemplate(template)
		return
	}
	m.root.Insert(filter, template)
}

func (m *matcher) AddDefaultTemplate(template *template) {
	m.defaultTemplate = template
}

// Match returns the template that matches the given graphite line
func (m *matcher) Match(line string) *template {
	tmpl := m.root.Search(line)
	if tmpl != nil {
		return tmpl
	}

	return m.defaultTemplate
}

// node is an item in a sorted k-ary tree.  Each child is sorted by its value.
// The special value of "*", is always last.
type node struct {
	value    string
	children nodes
	template *template
}

func (n *node) insert(values []string, template *template) {
	// Add the end, set the template
	if len(values) == 0 {
		n.template = template
		return
	}

	// See if the the current element already exists in the tree. If so, insert the
	// into that sub-tree
	for _, v := range n.children {
		if v.value == values[0] {
			v.insert(values[1:], template)
			return
		}
	}

	// New element, add it to the tree and sort the children
	newNode := &node{value: values[0]}
	n.children = append(n.children, newNode)
	sort.Sort(&n.children)

	// Now insert the rest of the tree into the new element
	newNode.insert(values[1:], template)
}

// Insert inserts the given string template into the tree.  The filter string is separated
// on "." and each part is used as the path in the tree.
func (n *node) Insert(filter string, template *template) {
	n.insert(strings.Split(filter, "."), template)
}

func (n *node) search(lineParts []string) *template {
	// Nothing to search
	if len(lineParts) == 0 || len(n.children) == 0 {
		return n.template
	}

	// If last element is a wildcard, don't include in this search since it's sorted
	// to the end but lexicographically it would not always be and sort.Search assumes
	// the slice is sorted.
	length := len(n.children)
	if n.children[length-1].value == "*" {
		length -= 1
	}

	// Find the index of child with an exact match
	i := sort.Search(length, func(i int) bool {
		return n.children[i].value == lineParts[0]
	})

	// Found an exact match, so search that child sub-tree
	if i < len(n.children) && n.children[i].value == lineParts[0] {
		return n.children[i].search(lineParts[1:])
	} else {
		// Not an exact match, see if we have a wildcard child to search
		if n.children[len(n.children)-1].value == "*" {
			return n.children[len(n.children)-1].search(lineParts[1:])
		}
	}
	return n.template
}

func (n *node) Search(line string) *template {
	return n.search(strings.Split(line, "."))
}

type nodes []*node

// Less returns a boolean indicating whether the filter at position j
// is less than the filter at position k.  Filters are order by string
// comparison of each component parts.  A wildcard value "*" is never
// less than a non-wildcard value.
//
// For example, the filters:
//             "*.*"
//             "servers.*"
//             "servers.localhost"
//             "*.localhost"
//
// Would be sorted as:
//             "servers.localhost"
//             "servers.*"
//             "*.localhost"
//             "*.*"
func (n *nodes) Less(j, k int) bool {
	if (*n)[j].value == "*" && (*n)[k].value != "*" {
		return false
	}

	if (*n)[j].value != "*" && (*n)[k].value == "*" {
		return true
	}

	return (*n)[j].value < (*n)[k].value
}

func (n *nodes) Swap(i, j int) { (*n)[i], (*n)[j] = (*n)[j], (*n)[i] }
func (n *nodes) Len() int      { return len(*n) }
