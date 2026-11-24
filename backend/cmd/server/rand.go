package main

import "crypto/rand"

// cryptoRandRead is crypto/rand.Read, aliased so newInstanceID reads cleanly.
func cryptoRandRead(b []byte) (int, error) { return rand.Read(b) }
