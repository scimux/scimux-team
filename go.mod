module github.com/scimux/scimux-team

go 1.27.1

// The minimum patched toolchain. The go directive above is the language
// floor and any 1.27.x satisfies it, including releases with known
// standard-library defects; CI pins this same version and govulncheck
// gates on it.
toolchain go1.27.4
