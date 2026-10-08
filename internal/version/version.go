// Package version holds the operator's version in one place.
package version

// Version is the operator's version. The Makefile stamps the same string into
// the binary with -ldflags, and a test asserts the two agree, because a
// version that is right in one of those two places and wrong in the other is
// worse than having neither.
const Version = "0.14.2"
