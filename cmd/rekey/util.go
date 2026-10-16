package main

import (
	"bytes"
	"strconv"
)

func trimJSON(raw []byte) []byte { return bytes.TrimSpace(raw) }

func strconvAtoi(s string) (int, error) { return strconv.Atoi(s) }
