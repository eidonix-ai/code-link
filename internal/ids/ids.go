// Package ids makes the random ids responses carry.
package ids

import (
	"crypto/rand"
	"math/big"
)

const idAlphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// New is prefix + 24 random alphanumerics ("msg_…", "toolu_…", "srvtoolu_…").
func New(prefix string) string {
	b := make([]byte, 24)
	n := big.NewInt(int64(len(idAlphabet)))
	for i := range b {
		k, err := rand.Int(rand.Reader, n)
		if err != nil {
			panic(err)
		}
		b[i] = idAlphabet[k.Int64()]
	}
	return prefix + string(b)
}
