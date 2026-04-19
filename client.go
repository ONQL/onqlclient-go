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
	Payload  string
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

type subscription struct {
	callback func(rid, keyword, payload string)
}

// Client is a concurrent-safe TCP client for the ONQL server.
type Client struct {
	conn    net.Conn
	writer  *bufio.Writer
	writeMu sync.Mutex

	pending   map[string]*pendingRequest
	pendingMu sync.Mutex

	subscriptions   map[string]*subscription
	subscriptionsMu sync.RWMutex

	timeout time.Duration
	done    chan struct{}
	closeOnce sync.Once

	// db is the default database name for Insert/Update/Delete/Onql.
	db string
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
		conn:          conn,
		writer:        bufio.NewWriterSize(conn, cfg.bufferSize),
		pending:       make(map[string]*pendingRequest),
		subscriptions: make(map[string]*subscription),
		timeout:       cfg.timeout,
		done:          make(chan struct{}),
	}

	go c.readLoop(cfg.bufferSize)
	return c, nil
}

// generateRequestID returns a random 8-character hex string, matching the
// Python driver's uuid4().hex[:8] approach.
func generateRequestID() string {
	b := make([]byte, 4) // 4 bytes = 8 hex chars
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("onql: failed to generate request ID: %v", err))
	}
	return hex.EncodeToString(b)
}

// readLoop continuously reads responses from the server and dispatches them
// to the appropriate pending request channel or subscription callback.
func (c *Client) readLoop(bufferSize int) {
	scanner := bufio.NewScanner(c.conn)
	scanner.Buffer(make([]byte, 0, bufferSize), bufferSize)
	scanner.Split(splitOnEOM)

	for scanner.Scan() {
		msg := scanner.Text()
		parts := splitFields(msg)
		if len(parts) != 3 {
			continue // malformed
		}

		rid, source, payload := parts[0], parts[1], parts[2]

		// Check subscriptions first.
		c.subscriptionsMu.RLock()
		sub, hasSub := c.subscriptions[rid]
		c.subscriptionsMu.RUnlock()
		if hasSub {
			// Dispatch in a separate goroutine so the reader is not blocked.
			go sub.callback(rid, source, payload)
			continue
		}

		// Otherwise deliver to a pending one-shot request.
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
				Payload:  payload,
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

// splitOnEOM is a bufio.SplitFunc that splits on the \x04 byte.
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

// splitFields splits a message on the \x1E delimiter into exactly its parts.
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

// sendRaw writes a framed message to the server.
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

// Subscribe opens a streaming subscription. The callback is invoked in its own
// goroutine for every frame the server sends with the returned request ID.
// The onquery and query parameters are JSON-encoded as the payload, matching
// the Python driver's protocol.
func (c *Client) Subscribe(onquery, query string, cb func(rid, keyword, payload string)) (string, error) {
	rid := generateRequestID()

	payloadMap := map[string]string{
		"onquery": onquery,
		"query":   query,
	}
	payloadBytes, err := json.Marshal(payloadMap)
	if err != nil {
		return "", fmt.Errorf("onql: failed to marshal subscribe payload: %w", err)
	}

	c.subscriptionsMu.Lock()
	c.subscriptions[rid] = &subscription{callback: cb}
	c.subscriptionsMu.Unlock()

	if err := c.sendRaw(rid, "subscribe", string(payloadBytes)); err != nil {
		c.subscriptionsMu.Lock()
		delete(c.subscriptions, rid)
		c.subscriptionsMu.Unlock()
		return "", err
	}

	return rid, nil
}

// Unsubscribe removes a subscription and notifies the server.
func (c *Client) Unsubscribe(rid string) error {
	// Remove local handler first to avoid races.
	c.subscriptionsMu.Lock()
	delete(c.subscriptions, rid)
	c.subscriptionsMu.Unlock()

	payloadMap := map[string]string{"rid": rid}
	payloadBytes, err := json.Marshal(payloadMap)
	if err != nil {
		return fmt.Errorf("onql: failed to marshal unsubscribe payload: %w", err)
	}

	// Best-effort: send unsubscribe frame to server.
	return c.sendRaw(rid, "unsubscribe", string(payloadBytes))
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
// ---------------------------------------------------------------------------

// Setup sets the default database name used by Insert/Update/Delete/Onql.
// Returns the receiver so calls can be chained.
func (c *Client) Setup(db string) *Client {
	c.db = db
	return c
}

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

// WithIDs sets the explicit record IDs for Update/Delete.
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

// Insert inserts one record or a slice of records into table.
// `data` is marshalled as the "records" field; the returned bytes are the
// decoded "data" field from the server envelope.
func (c *Client) Insert(table string, data interface{}) (json.RawMessage, error) {
	payload, err := json.Marshal(map[string]interface{}{
		"db": c.db, "table": table, "records": data,
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

// Update updates records matching `query`.
// Optional parameters: WithProtopass(...), WithIDs(...).
func (c *Client) Update(table string, data, query interface{}, opts ...OnqlOption) (json.RawMessage, error) {
	o := applyOptions(opts)
	payload, err := json.Marshal(map[string]interface{}{
		"db":        c.db,
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

// Delete deletes records matching `query`.
// Optional parameters: WithProtopass(...), WithIDs(...).
func (c *Client) Delete(table string, query interface{}, opts ...OnqlOption) (json.RawMessage, error) {
	o := applyOptions(opts)
	payload, err := json.Marshal(map[string]interface{}{
		"db":        c.db,
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

// Build replaces `$1`, `$2`, ... placeholders in `query` with the supplied
// values. Strings are double-quoted; numeric and boolean values are inlined
// verbatim.
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
