// Package compress provides compression algorithms for fido persistence stores.
package compress

import (
	"github.com/klauspost/compress/s2"
	"github.com/klauspost/compress/zstd"
)

// Compressor compresses and decompresses data.
type Compressor interface {
	Encode(data []byte) ([]byte, error)
	Decode(data []byte) ([]byte, error)
	Extension() string
}

type none struct{}

// None returns a pass-through compressor (no compression).
func None() Compressor { return none{} }

func (none) Encode(data []byte) ([]byte, error) { return data, nil }
func (none) Decode(data []byte) ([]byte, error) { return data, nil }
func (none) Extension() string                  { return "" }

type s2c struct{}

// S2 returns a fast compressor using S2 (improved Snappy).
func S2() Compressor { return s2c{} }

func (s2c) Encode(data []byte) ([]byte, error) { return s2.Encode(nil, data), nil }
func (s2c) Decode(data []byte) ([]byte, error) { return s2.Decode(nil, data) }
func (s2c) Extension() string                  { return ".s" }

type zstdc struct {
	enc *zstd.Encoder
	dec *zstd.Decoder
}

// Zstd returns a compressor using Zstandard.
// Level: 1 (fastest) to 4 (best compression).
func Zstd(level int) Compressor {
	lvl := zstd.SpeedDefault
	if level <= 1 {
		lvl = zstd.SpeedFastest
	} else if level >= 4 {
		lvl = zstd.SpeedBestCompression
	}
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(lvl)) //nolint:errcheck // options are valid
	dec, _ := zstd.NewReader(nil)                             //nolint:errcheck // options are valid
	return &zstdc{enc: enc, dec: dec}
}

func (z *zstdc) Encode(data []byte) ([]byte, error) { return z.enc.EncodeAll(data, nil), nil }
func (z *zstdc) Decode(data []byte) ([]byte, error) { return z.dec.DecodeAll(data, nil) }
func (*zstdc) Extension() string                    { return ".z" }
