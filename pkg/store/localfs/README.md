# persist/localfs

Local filesystem persistence using Go's gob encoding.

## Features

- Zero dependencies beyond stdlib
- Automatic directory management
- Per-item file storage for crash safety
- Buffered I/O for performance
- Works across all platforms

## Usage

```go
import (
    "github.com/codeGROOVE-dev/sfcache"
    "github.com/codeGROOVE-dev/sfcache/persist/localfs"
)

// Uses OS cache directory (e.g., ~/.cache/myapp on Linux)
p, _ := localfs.New[string, User]("myapp", "")

// Or specify custom directory
p, _ := localfs.New[string, User]("myapp", "/tmp/my-cache")

cache, _ := sfcache.New[string, User](ctx,
    sfcache.WithPersistence(p))
```

## Storage Location

Files are stored in subdirectories based on key hash to avoid filesystem limits:
- Linux/macOS: `~/.cache/myapp/XX/key`
- Windows: `%LocalAppData%\myapp\XX\key`

Where `XX` is the first 2 hex digits of the key's hash.

## Key Constraints

- Maximum key length: 127 characters
- Keys are sanitized for filesystem safety
- No path traversal sequences allowed
