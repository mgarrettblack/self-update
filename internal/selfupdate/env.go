package selfupdate

import "github.com/spf13/viper"

// Viper's global instance is bound to the environment here rather than in a
// caller, so allowHTTP behaves the same however the library is embedded. Without
// AutomaticEnv, viper.GetBool reads only explicitly-set keys and would return
// false no matter what the environment holds.
func init() {
	viper.AutomaticEnv()
}

// allowHTTPKey is read through viper's AutomaticEnv, which uppercases it, so
// this resolves the SELFUPDATE_ALLOW_HTTP environment variable. No SetEnvPrefix
// is needed.
const allowHTTPKey = "selfupdate_allow_http"

// allowHTTP reports whether plaintext HTTP is permitted. Off unless
// SELFUPDATE_ALLOW_HTTP parses as a true bool. Local development only: an
// attacker who can rewrite plaintext responses controls what this process
// executes next.
//
// There is no manifest signature check to fall back on — it is not implemented
// (see docs/platforms/known-gaps.md). Over plaintext, the only integrity
// control left is the artifact's SHA-256, and that digest comes from the same
// manifest an attacker would be rewriting. Treat this switch as disabling
// update security entirely, not weakening it.
func allowHTTP() bool {
	return viper.GetBool(allowHTTPKey)
}
