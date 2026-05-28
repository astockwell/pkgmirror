// Copyright 2026 Alex Stockwell and pkgmirror contributors.
// Portions Copyright 2021 The Gitea Authors.
// SPDX-License-Identifier: MIT
//
// Ported from forgejo/modules/packages/rubygems/marshal_test.go (MIT).
// Bytes expected by each case are exactly the upstream golden values —
// any divergence would indicate the encoder is out of spec with Ruby.

package rubygems

import (
	"bytes"
	"errors"
	"testing"
)

func TestMarshalEncoder(t *testing.T) {
	cases := []struct {
		Name     string
		Value    any
		Expected []byte
		Err      error
	}{
		{Name: "nil", Value: nil, Expected: []byte{4, 8, 0x30}},
		{Name: "true", Value: true, Expected: []byte{4, 8, 'T'}},
		{Name: "false", Value: false, Expected: []byte{4, 8, 'F'}},
		{Name: "0", Value: 0, Expected: []byte{4, 8, 'i', 0}},
		{Name: "1", Value: 1, Expected: []byte{4, 8, 'i', 6}},
		{Name: "-1", Value: -1, Expected: []byte{4, 8, 'i', 0xfa}},
		{Name: "0x1fffffff", Value: 0x1fffffff, Expected: []byte{4, 8, 'i', 4, 0xff, 0xff, 0xff, 0x1f}},
		{Name: "over-range", Value: 0x41000000, Err: ErrInvalidIntRange},
		{Name: "string", Value: "test", Expected: []byte{4, 8, 'I', '"', 9, 't', 'e', 's', 't', 6, ':', 6, 'E', 'T'}},
		{Name: "array", Value: []int{1, 2}, Expected: []byte{4, 8, '[', 7, 'i', 6, 'i', 7}},
		{
			Name:     "user_marshal",
			Value:    &RubyUserMarshal{Name: "Test", Value: 4},
			Expected: []byte{4, 8, 'U', ':', 9, 'T', 'e', 's', 't', 'i', 9},
		},
		{
			Name:     "user_def",
			Value:    &RubyUserDef{Name: "Test", Value: 4},
			Expected: []byte{4, 8, 'u', ':', 9, 'T', 'e', 's', 't', 9, 4, 8, 'i', 9},
		},
		{
			Name:     "object",
			Value:    &RubyObject{Name: "Test", Member: map[string]any{"test": 4}},
			Expected: []byte{4, 8, 'o', ':', 9, 'T', 'e', 's', 't', 6, ':', 9, 't', 'e', 's', 't', 'i', 9},
		},
		{
			Name:  "unsupported_struct",
			Value: &struct{ Name string }{"test"},
			Err:   ErrUnsupportedType,
		},
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			var b bytes.Buffer
			err := NewMarshalEncoder(&b).Encode(tc.Value)
			if !errors.Is(err, tc.Err) {
				t.Fatalf("err=%v want %v", err, tc.Err)
			}
			if tc.Err == nil && !bytes.Equal(b.Bytes(), tc.Expected) {
				t.Fatalf("got %v want %v", b.Bytes(), tc.Expected)
			}
		})
	}
}

// TestMarshalEncoder_SymbolDeduplication verifies that repeated symbols
// get encoded as symbol-link references rather than fresh symbols. This
// is what makes the /specs.4.8.gz response a few bytes smaller per
// package and matches Ruby Marshal semantics exactly.
func TestMarshalEncoder_SymbolDeduplication(t *testing.T) {
	v := []any{
		&RubyUserMarshal{Name: "Gem::Version", Value: []string{"1.0.0"}},
		&RubyUserMarshal{Name: "Gem::Version", Value: []string{"2.0.0"}},
	}
	var b bytes.Buffer
	if err := NewMarshalEncoder(&b).Encode(v); err != nil {
		t.Fatalf("encode: %v", err)
	}
	out := b.Bytes()
	// First "Gem::Version" should be a fresh symbol (':'), the second a
	// symbol-link (';'). Find them in order.
	firstSym := bytes.IndexByte(out, typeSymbol)
	firstLink := bytes.IndexByte(out, typeSymbolLink)
	if firstSym < 0 || firstLink < 0 || firstLink < firstSym {
		t.Fatalf("expected one ':' followed by ';', got %v", out)
	}
}
