package observability

// Version is the build version, injected at link time via -ldflags.
// It is reported by `kiln version`, by GET /api/v1/version, and is what the
// web frontend compares against to detect a skewed server/UI pair.
var Version = "dev"
