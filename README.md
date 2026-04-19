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

    if _, err := client.Insert("mydb", "users",
        User{ID: "u1", Name: "John", Age: 30}); err != nil {
        log.Fatal(err)
    }

    var users []User
    if _, err := client.Onql("mydb.users[age>18]", &users); err != nil {
        log.Fatal(err)
    }
    fmt.Println(users)

    client.Update("mydb", "users",
        map[string]any{"age": 31},
        client.Build("mydb.users[id=$1].id", "u1"))

    client.Delete("mydb", "users", "", onql.WithIDs([]string{"u1"}))
}
```

## API Reference

### `onql.Connect(host, port, ...opts) (*Client, error)`

Creates and returns a connected client.

### `client.SendRequest(keyword, payload) (*Response, error)`

Sends a raw request frame and waits for the response.

### `client.Close() error`

Closes the connection.

## Direct ORM-style API

On top of raw `SendRequest`, the client exposes convenience methods that build
the standard payload envelopes for `Insert` / `Update` / `Delete` / `Onql` and
unwrap the server's `{error, data}` response automatically.

`db` is passed explicitly to `Insert` / `Update` / `Delete`. `Onql` takes a
fully-qualified ONQL expression (which already includes the db name), so no
separate db argument is needed.

`query` arguments are **ONQL expression strings**, e.g.
`mydb.users[id="u1"].id`. Use `client.Build(template, values...)` to
substitute `$1, $2, ...`.

### `client.Insert(db, table string, data interface{}) (json.RawMessage, error)`

Insert a **single** record.

```go
client.Insert("mydb", "users", map[string]any{
    "id": "u1", "name": "John", "age": 30,
})
```

### `client.Update(db, table string, data interface{}, query string, ...opts) (json.RawMessage, error)`

Update records matching `query` (or the explicit IDs supplied via `WithIDs`).

| Option | Default | Description |
|--------|---------|-------------|
| `WithProtopass(string)` | `"default"` | Proto-pass profile |
| `WithIDs([]string)` | `[]` | Explicit record IDs (alternative to `query`) |

```go
// Via ONQL query
client.Update("mydb", "users",
    map[string]any{"age": 31},
    client.Build("mydb.users[id=$1].id", "u1"))

// Via explicit IDs
client.Update("mydb", "users",
    map[string]any{"age": 31}, "",
    onql.WithIDs([]string{"u1"}))
```

### `client.Delete(db, table, query string, ...opts) (json.RawMessage, error)`

Delete records matching `query` (or `WithIDs`).

```go
client.Delete("mydb", "users",
    client.Build("mydb.users[id=$1].id", "u1"))

client.Delete("mydb", "users", "", onql.WithIDs([]string{"u1"}))
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
_, err := client.Onql("mydb.users[age>18]", &users)
```

### `client.Build(query string, values ...interface{}) string`

Replace `$1`, `$2`, … placeholders with values. Strings are double-quoted;
numeric and boolean values are inlined verbatim.

```go
q := client.Build("mydb.users[name=$1 and age>$2]", "John", 18)
// -> mydb.users[name="John" and age>18]
var rows []User
_, err := client.Onql(q, &rows)
```

## Protocol

```
<request_id>\x1E<keyword>\x1E<payload>\x04
```

- `\x1E` — field delimiter
- `\x04` — end-of-message marker

## License

MIT
