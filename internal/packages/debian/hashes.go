// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// SPDX-License-Identifier: MIT

package debian

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
)

// hashAll returns the size + four hex-encoded checksums of body.
// apt's Release file references every Packages variant by all four
// hashes; we compute them in a single pass so the byte slice is only
// read once.
func hashAll(body []byte) IndexHashes {
	m := md5.Sum(body)
	s1 := sha1.Sum(body)
	s256 := sha256.Sum256(body)
	s512 := sha512.Sum512(body)
	return IndexHashes{
		Size:       int64(len(body)),
		MD5:        hex.EncodeToString(m[:]),
		SHA1:       hex.EncodeToString(s1[:]),
		SHA256:     hex.EncodeToString(s256[:]),
		SHA512Hash: hex.EncodeToString(s512[:]),
	}
}
