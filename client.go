package onql

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	eom       = '\x04' // End-of-Message delimiter
	delimiter = "\x1E" // Field delimiter (record separator)
)

// Response represents a parsed server response.
type Response struct {
	RequestID string
	Source    string
	Payload   string
}

// Option configures the client.
type Option func(*config)

type config struct {
	timeout    time.Duration
	bufferSize int
}

// WithTimeout sets the default request timeout.
func WithTimeout(d time.Duration) Option {
	return func(c *config) {
		c.timeout = d
	}
}

// WithBufferSize sets the read buffer size in bytes.
func WithBufferSize(size int) Option {
	return func(c *config) {
		c.bufferSize = size
	}
}

type pendingRequest struct {
	ch chan *Response
}

// Client is a concurrent-safe TCP client for the ONQL server.
type Client struct {
	conn    net.Conn
	writer  *bufio.Writer
	writeMu sync.Mutex

	pending   map[string]*pendingRequest
	pendingMu sync.Mutex

	timeout   time.Duration
	done      chan struct{}
	closeOnce sync.Once
}

// Connect establishes a TCP connection to the ONQL server and starts
// the background response reader.
func Connect(host string, port int, opts ...Option) (*Client, error) {
	cfg := &config{
		timeout:    10 * time.Second,
		bufferSize: 16 * 1024 * 1024, // 16 MB
	}
	for _, o := range opts {
		o(cfg)
	}

	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	conn, err := net.DialTimeout("tcp", addr, cfg.timeout)
	if err != nil {
		return nil, fmt.Errorf("onql: could not connect to %s: %w", addr, err)
	}

	c := &Client{
		conn:    conn,
		writer:  bufio.NewWriterSize(conn, cfg.bufferSize),
		pending: make(map[string]*pendingRequest),
		timeout: cfg.timeout,
		done:    make(chan struct{}),
	}

	go c.readLoop(cfg.bufferSize)
	return c, nil
}

// generateRequestID returns a random 8-character hex string.
func generateRequestID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("onql: failed to generate request ID: %v", err))
	}
	return hex.EncodeToString(b)
}

// readLoop continuously reads responses from the server and dispatches them
// to the appropriate pending request channel.
func (c *Client) readLoop(bufferSize int) {
	scanner := bufio.NewScanner(c.conn)
	scanner.Buffer(make([]byte, 0, bufferSize), bufferSize)
	scanner.Split(splitOnEOM)

	for scanner.Scan() {
		msg := scanner.Text()
		parts := splitFields(msg)
		if len(parts) != 3 {
			continue
		}
		rid, source, payload := parts[0], parts[1], parts[2]

		c.pendingMu.Lock()
		pr, ok := c.pending[rid]
		if ok {
			delete(c.pending, rid)
		}
		c.pendingMu.Unlock()

		if ok {
			pr.ch <- &Response{
				RequestID: rid,
				Source:    source,
				Payload:   payload,
			}
		}
	}

	// Connection lost or closed: wake up all pending requests.
	c.pendingMu.Lock()
	for rid, pr := range c.pending {
		close(pr.ch)
		delete(c.pending, rid)
	}
	c.pendingMu.Unlock()

	close(c.done)
}

func splitOnEOM(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for i := 0; i < len(data); i++ {
		if data[i] == eom {
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func splitFields(s string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\x1E' {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}

func (c *Client) sendRaw(rid, keyword, payload string) error {
	frame := rid + delimiter + keyword + delimiter + payload + string(eom)

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if _, err := c.writer.WriteString(frame); err != nil {
		return fmt.Errorf("onql: write error: %w", err)
	}
	return c.writer.Flush()
}

// SendRequest sends a request and waits for the response using the default timeout.
func (c *Client) SendRequest(keyword, payload string) (*Response, error) {
	return c.SendRequestTimeout(keyword, payload, c.timeout)
}

// SendRequestTimeout sends a request and waits for the response with a custom timeout.
func (c *Client) SendRequestTimeout(keyword, payload string, timeout time.Duration) (*Response, error) {
	rid := generateRequestID()
	pr := &pendingRequest{ch: make(chan *Response, 1)}

	c.pendingMu.Lock()
	c.pending[rid] = pr
	c.pendingMu.Unlock()

	if err := c.sendRaw(rid, keyword, payload); err != nil {
		c.pendingMu.Lock()
		delete(c.pending, rid)
		c.pendingMu.Unlock()
		return nil, err
	}

	select {
	case resp, ok := <-pr.ch:
		if !ok {
			return nil, errors.New("onql: connection lost")
		}
		return resp, nil
	case <-time.After(timeout):
		c.pendingMu.Lock()
		delete(c.pending, rid)
		c.pendingMu.Unlock()
		return nil, fmt.Errorf("onql: request %s timed out after %v", rid, timeout)
	case <-c.done:
		return nil, errors.New("onql: client closed")
	}
}

// Close shuts down the connection and stops the reader loop.
func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() {
		err = c.conn.Close()
	})
	return err
}

// ---------------------------------------------------------------------------
// Direct ORM-style API (Insert / Update / Delete / Onql / Build)
//
// `query` arguments are ONQL expression *strings*, e.g.
//    "mydb.users[id=\"u1\"].id"
//    "mydb.orders[status=\"pending\"]"
// Use Client.Build(template, values...) to substitute $1, $2, ...
// ---------------------------------------------------------------------------

// OnqlOption configures an optional parameter on an ORM-style call.
type OnqlOption func(*onqlOptions)

type onqlOptions struct {
	protopass string
	ctxKey    string
	ctxValues []string
	ids       []string
}

// WithProtopass sets a custom proto-pass profile.
func WithProtopass(p string) OnqlOption {
	return func(o *onqlOptions) { o.protopass = p }
}

// WithContext sets the context key and values for an Onql call.
func WithContext(key string, values []string) OnqlOption {
	return func(o *onqlOptions) {
		o.ctxKey = key
		o.ctxValues = values
	}
}

// WithIDs supplies explicit record IDs as an alternative to a query string
// on Update/Delete.
func WithIDs(ids []string) OnqlOption {
	return func(o *onqlOptions) { o.ids = ids }
}

func applyOptions(opts []OnqlOption) *onqlOptions {
	o := &onqlOptions{protopass: "default", ctxValues: []string{}, ids: []string{}}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// envelope is the standard {error, data} response envelope.
type envelope struct {
	Error string          `json:"error"`
	Data  json.RawMessage `json:"data"`
}

// processResult parses the standard {error, data} envelope, returning the raw
// data bytes. If `out` is non-nil, data is unmarshalled into it.
func processResult(raw string, out interface{}) (json.RawMessage, error) {
	var env envelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return nil, fmt.Errorf("onql: %s", raw)
	}
	if env.Error != "" {
		return nil, errors.New(env.Error)
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return env.Data, fmt.Errorf("onql: failed to unmarshal data: %w", err)
		}
	}
	return env.Data, nil
}

// Insert inserts a single record into `db.table`.
// `data` can be any JSON-serialisable value — struct, map, etc.
func (c *Client) Insert(db, table string, data interface{}) (json.RawMessage, error) {
	payload, err := json.Marshal(map[string]interface{}{
		"db":      db,
		"table":   table,
		"records": data,
	})
	if err != nil {
		return nil, err
	}
	resp, err := c.SendRequest("insert", string(payload))
	if err != nil {
		return nil, err
	}
	return processResult(resp.Payload, nil)
}

// Update updates records in `db.table` matching `query` (or the explicit IDs
// supplied via WithIDs). `query` is an ONQL expression string — pass "" when
// using WithIDs.
// Optional parameters: WithProtopass(...), WithIDs(...).
func (c *Client) Update(db, table string, data interface{}, query string, opts ...OnqlOption) (json.RawMessage, error) {
	o := applyOptions(opts)
	payload, err := json.Marshal(map[string]interface{}{
		"db":        db,
		"table":     table,
		"records":   data,
		"query":     query,
		"protopass": o.protopass,
		"ids":       o.ids,
	})
	if err != nil {
		return nil, err
	}
	resp, err := c.SendRequest("update", string(payload))
	if err != nil {
		return nil, err
	}
	return processResult(resp.Payload, nil)
}

// Delete deletes records in `db.table` matching `query` (or WithIDs).
// Optional parameters: WithProtopass(...), WithIDs(...).
func (c *Client) Delete(db, table, query string, opts ...OnqlOption) (json.RawMessage, error) {
	o := applyOptions(opts)
	payload, err := json.Marshal(map[string]interface{}{
		"db":        db,
		"table":     table,
		"query":     query,
		"protopass": o.protopass,
		"ids":       o.ids,
	})
	if err != nil {
		return nil, err
	}
	resp, err := c.SendRequest("delete", string(payload))
	if err != nil {
		return nil, err
	}
	return processResult(resp.Payload, nil)
}

// Onql executes a raw ONQL query. When `out` is non-nil, the decoded "data"
// field is unmarshalled into it (pass a pointer to a struct or slice).
// Optional parameters: WithProtopass(...), WithContext(key, values).
func (c *Client) Onql(query string, out interface{}, opts ...OnqlOption) (json.RawMessage, error) {
	o := applyOptions(opts)
	payload, err := json.Marshal(map[string]interface{}{
		"query":     query,
		"protopass": o.protopass,
		"ctxkey":    o.ctxKey,
		"ctxvalues": o.ctxValues,
	})
	if err != nil {
		return nil, err
	}
	resp, err := c.SendRequest("onql", string(payload))
	if err != nil {
		return nil, err
	}
	return processResult(resp.Payload, out)
}

// Build replaces $1, $2, ... placeholders in `query` with the supplied values.
// Strings are double-quoted; numeric and boolean values are inlined verbatim.
func (c *Client) Build(query string, values ...interface{}) string {
	for i, value := range values {
		placeholder := "$" + strconv.Itoa(i+1)
		var replacement string
		switch v := value.(type) {
		case string:
			replacement = `"` + v + `"`
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			replacement = fmt.Sprintf("%d", v)
		case float32, float64:
			replacement = fmt.Sprintf("%v", v)
		case bool:
			replacement = strconv.FormatBool(v)
		default:
			replacement = fmt.Sprint(v)
		}
		query = strings.ReplaceAll(query, placeholder, replacement)
	}
	return query
}
