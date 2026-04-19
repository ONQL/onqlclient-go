# ONQL Go Driver

Official Go client for the ONQL database server.

## Installation

### Latest tagged release (via the Go module proxy)

```bash
go get github.com/ONQL/onqlclient-go
```

### A specific version

```bash
go get github.com/ONQL/onqlclient-go@v0.1.0
```

### Latest `main` (no tag required)

```bash
go get github.com/ONQL/onqlclient-go@main
```

## Quick Start

```go
package main

import (
	"fmt"
	"log"

	onql "github.com/ONQL/onqlclient-go"
)

func main() {
	client, err := onql.Connect("localhost", 5656)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	// Execute a query
	result, err := client.SendRequest("onql", `{"db":"mydb","table":"users","query":"name = \"John\""}`)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(result.Payload)

	// Subscribe to live updates
	rid, err := client.Subscribe("", `name = "John"`, func(rid, keyword, payload string) {
		fmt.Println("Update:", payload)
	})
	if err != nil {
		log.Fatal(err)
	}

	// Unsubscribe
	client.Unsubscribe(rid)
}
```

## API Reference

### `onql.Connect(host, port, ...opts)`

Creates and returns a connected client.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `host` | `string` | `"localhost"` | Server hostname |
| `port` | `int` | `5656` | Server port |

### Options

```go
onql.Connect("localhost", 5656,
	onql.WithTimeout(10 * time.Second),
	onql.WithBufferSize(16 * 1024 * 1024),
)
```

### `client.SendRequest(keyword, payload) (*Response, error)`

Sends a request and waits for a response.

### `client.Subscribe(onquery, query, callback) (string, error)`

Opens a streaming subscription. Returns the subscription ID.

### `client.Unsubscribe(rid) error`

Stops receiving events for a subscription.

### `client.Close() error`

Closes the connection.

## Direct ORM-style API

In addition to raw `SendRequest`, the client exposes convenience methods that
build the standard payload envelopes for common operations and unwrap the
server's `{error, data}` response automatically.

Call `client.Setup(db)` once to bind a default database name; every subsequent
`Insert` / `Update` / `Delete` / `Onql` call will use it.

### `client.Setup(db string) *Client`

Sets the default database. Returns the receiver, so calls can be chained.

```go
client.Setup("mydb")
```

### `client.Insert(table string, data interface{}) (json.RawMessage, error)`

Insert one record or a slice of records.

| Parameter | Type | Description |
|-----------|------|-------------|
| `table` | `string` | Target table |
| `data` | `any` | A struct/map, or a slice of them |

Returns the decoded `data` field from the server envelope as raw JSON.
Returns an error if the server sets a non-empty `error` field.

```go
_, err := client.Insert("users", map[string]any{"name": "John", "age": 30})
_, err := client.Insert("users", []map[string]any{{"name": "A"}, {"name": "B"}})
```

### `client.Update(table, data, query, ...opts) (json.RawMessage, error)`

Update records matching `query`.

| Option | Default | Description |
|--------|---------|-------------|
| `WithProtopass(string)` | `"default"` | Proto-pass profile |
| `WithIDs([]string)` | `[]` | Explicit record IDs |

```go
client.Update("users", map[string]any{"age": 31}, map[string]any{"name": "John"})
client.Update("users", data, query, onql.WithProtopass("admin"))
```

### `client.Delete(table, query, ...opts) (json.RawMessage, error)`

Delete records matching `query`. Accepts the same options as `Update`.

```go
client.Delete("users", map[string]any{"active": false})
```

### `client.Onql(query string, out interface{}, ...opts) (json.RawMessage, error)`

Run a raw ONQL query. If `out` is non-nil, the decoded `data` field is
unmarshalled into it (pass a pointer to a struct or slice).

| Option | Default | Description |
|--------|---------|-------------|
| `WithProtopass(string)` | `"default"` | Proto-pass profile |
| `WithContext(key, values)` | `"", []` | Context key / values |

```go
var users []User
_, err := client.Onql("select * from users where age > 18", &users)
```

### `client.Build(query string, values ...interface{}) string`

Replace `$1`, `$2`, … placeholders with values. Strings are double-quoted;
numeric and boolean values are inlined verbatim.

```go
q := client.Build("select * from users where name = $1 and age > $2", "John", 18)
// -> `select * from users where name = "John" and age > 18`
var rows []User
_, err := client.Onql(q, &rows)
```

### Full example

```go
package main

import (
    "fmt"
    "log"

    onql "github.com/ONQL/onqlclient-go"
)

type User struct {
    Name string `json:"name"`
    Age  int    `json:"age"`
}

func main() {
    client, err := onql.Connect("localhost", 5656)
    if err != nil { log.Fatal(err) }
    defer client.Close()

    client.Setup("mydb")

    if _, err := client.Insert("users", User{Name: "John", Age: 30}); err != nil {
        log.Fatal(err)
    }

    var users []User
    if _, err := client.Onql(
        client.Build("select * from users where age >= $1", 18),
        &users,
    ); err != nil {
        log.Fatal(err)
    }
    fmt.Println(users)

    client.Update("users", map[string]any{"age": 31}, map[string]any{"name": "John"})
    client.Delete("users", map[string]any{"name": "John"})
}
```

## Protocol

The client communicates over TCP using a delimiter-based message format:

```
<request_id>\x1E<keyword>\x1E<payload>\x04
```

- `\x1E` — field delimiter
- `\x04` — end-of-message marker

## License

MIT
