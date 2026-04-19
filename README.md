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

type User struct {
    ID   string `json:"id"`
    Name string `json:"name"`
    Age  int    `json:"age"`
}

func main() {
    client, err := onql.Connect("localhost", 5656)
    if err != nil { log.Fatal(err) }
    defer client.Close()

    if _, err := client.Insert("mydb.users",
        User{ID: "u1", Name: "John", Age: 30}); err != nil {
        log.Fatal(err)
    }

    var users []User
    if _, err := client.Onql("select * from mydb.users where age > 18", &users); err != nil {
        log.Fatal(err)
    }
    fmt.Println(users)

    client.Update("mydb.users.u1", map[string]any{"age": 31})
    client.Delete("mydb.users.u1")
}
```

## API Reference

### `onql.Connect(host, port, ...opts) (*Client, error)`

Creates and returns a connected client.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `host` | `string` | — | Server hostname |
| `port` | `int` | — | Server port |

### Options

```go
onql.Connect("localhost", 5656,
    onql.WithTimeout(10 * time.Second),
    onql.WithBufferSize(16 * 1024 * 1024),
)
```

### `client.SendRequest(keyword, payload) (*Response, error)`

Sends a raw request frame and waits for the response.

### `client.Close() error`

Closes the connection.

## Direct ORM-style API

On top of raw `SendRequest`, the client exposes convenience methods that build
the standard payload envelopes for `Insert` / `Update` / `Delete` / `Onql` and
unwrap the server's `{error, data}` response automatically — returning an
error on a non-empty `error`, or the decoded `data` bytes on success.

The `path` argument is a **dotted string** identifying what you're operating
on:

| Path shape | Meaning |
|------------|---------|
| `"mydb.users"` | The `users` table in database `mydb` (used by `Insert`) |
| `"mydb.users.u1"` | The record with id `u1` (used by `Update` / `Delete`) |

### `client.Insert(path string, data interface{}) (json.RawMessage, error)`

Insert a **single** record.

```go
client.Insert("mydb.users",
    map[string]any{"id": "u1", "name": "John", "age": 30})
```

### `client.Update(path string, data interface{}, ...opts) (json.RawMessage, error)`

Update the record at `path`. Options: `WithProtopass(string)`.

```go
client.Update("mydb.users.u1", map[string]any{"age": 31})
client.Update("mydb.users.u1",
    map[string]any{"active": false},
    onql.WithProtopass("admin"))
```

### `client.Delete(path string, ...opts) (json.RawMessage, error)`

Delete the record at `path`.

```go
client.Delete("mydb.users.u1")
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
_, err := client.Onql("select * from mydb.users where age > 18", &users)
```

### `client.Build(query string, values ...interface{}) string`

Replace `$1`, `$2`, … placeholders with values. Strings are double-quoted;
numeric and boolean values are inlined verbatim.

```go
q := client.Build("select * from mydb.users where name = $1 and age > $2",
    "John", 18)
// -> select * from mydb.users where name = "John" and age > 18
var rows []User
_, err := client.Onql(q, &rows)
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
