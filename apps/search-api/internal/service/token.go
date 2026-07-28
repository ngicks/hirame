package service

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"connectrpc.com/connect"
)

// pageToken is the state a search cursor carries between requests.
//
// It holds the query it belongs to as well as the offset, because the contract
// is that pairing a token with a different query fails rather than returning
// another query's page. Only a digest is stored: the token travels through a
// URL and a browser cache, and the offset is the only part a server needs to
// read back.
//
// It is not signed. There is no user authentication in this deployment (D-010)
// and nothing behind the cursor is privileged — the worst a forged token can
// do is ask for an offset the caller could have paged to anyway.
type pageToken struct {
	Query  string `json:"q"`
	Offset int64  `json:"o"`
}

// queryDigest identifies a query without carrying it.
//
// Truncated to 8 bytes: this detects a client pairing a token with the wrong
// query, which is a mistake rather than an attack, and a full digest would
// triple the token for no added protection.
func queryDigest(normalizedQuery string) string {
	sum := sha256.Sum256([]byte(normalizedQuery))
	return hex.EncodeToString(sum[:8])
}

// maxPageOffset is how deep paging may go. It is well inside int32 so the
// narrowing at the query boundary can never wrap.
const maxPageOffset = int64(1) << 30

func encodePageToken(normalizedQuery string, offset int64) (string, error) {
	body, err := json.Marshal(pageToken{Query: queryDigest(normalizedQuery), Offset: offset})
	if err != nil {
		return "", fmt.Errorf("encode page token: %w", err)
	}
	// RawURL so the token survives a query string without escaping, which is
	// where the GUI keeps it.
	return base64.RawURLEncoding.EncodeToString(body), nil
}

// decodePageToken reads an offset out of a token, refusing one issued for a
// different query.
//
// Every failure is INVALID_ARGUMENT, including a corrupt token: the client
// cannot repair either case by retrying, and the proto documents the mismatch
// as exactly this code.
func decodePageToken(token, normalizedQuery string) (int64, error) {
	if token == "" {
		return 0, nil
	}
	body, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, connect.NewError(
			connect.CodeInvalidArgument,
			errors.New("page_token is not a token this server issued"),
		)
	}
	var decoded pageToken
	if err := json.Unmarshal(body, &decoded); err != nil {
		return 0, connect.NewError(
			connect.CodeInvalidArgument,
			errors.New("page_token is not a token this server issued"),
		)
	}
	if decoded.Query != queryDigest(normalizedQuery) {
		return 0, connect.NewError(
			connect.CodeInvalidArgument,
			errors.New("page_token belongs to a different query"),
		)
	}
	// Bounded at both ends: the offset is narrowed to int32 for the query, and
	// a value past that would wrap to a negative OFFSET the database rejects,
	// turning a bad token into an internal error instead of a bad request.
	if decoded.Offset < 0 || decoded.Offset > maxPageOffset {
		return 0, connect.NewError(
			connect.CodeInvalidArgument,
			errors.New("page_token carries an offset outside the searchable range"),
		)
	}
	return decoded.Offset, nil
}
