package hash

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

type Digest string

func Bytes(b []byte) Digest {
	h := sha256.Sum256(b)
	return Digest(hex.EncodeToString(h[:]))
}

func Document(b []byte) Digest {
	return Bytes(b)
}

func CombineSet(parts ...string) Digest {
	cp := make([]string, len(parts))
	copy(cp, parts)
	sort.Strings(cp)
	joined := strings.Join(cp, "|")
	return Bytes([]byte(joined))
}
