package main

import "errors"

// errNotImplemented marks every code path in this package that has not yet
// been given a real implementation. Each stubbed function wraps it into the
// error it returns so a caller (and a test) can tell "not implemented yet"
// apart from "implemented and rejected this input" and "implemented and
// accepted it". This file, and the sentinel it declares, are removed once
// every stub below has a real implementation; nothing outside this package
// depends on it.
var errNotImplemented = errors.New("not implemented")
