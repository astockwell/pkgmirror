package rubygems_test

import (
	"bytes"
	"compress/zlib"
	"io"
)

func decompressZlib(b []byte) (io.ReadCloser, error) {
	return zlib.NewReader(bytes.NewReader(b))
}
