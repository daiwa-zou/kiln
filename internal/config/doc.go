// Package config loads kiln's configuration.
//
// Precedence is flags > environment > config file > defaults. Environment is the
// primary surface because the deployment target is containers; the file exists
// for local development.
//
// Every secret-bearing variable also accepts a _FILE suffix pointing at a path
// (KILN_DATABASE_URL_FILE, KILN_STORAGE_SECRET_KEY_FILE, KILN_MASTER_KEY_FILE,
// KILN_ANTHROPIC_API_KEY_FILE), so Docker secrets and mounted Kubernetes secrets
// work without putting credentials in the environment. The _FILE form wins when
// both are set.
package config
