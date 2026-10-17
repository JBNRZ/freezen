package zen

import "time"

// Where Zen's free models answer, what each model is, and where the docs say
// what each model is asked at (provider.mjs, models.mjs).

// PluginVersion is the release of the plugin this server ports
// (@magpie-community/opencode-zen-free-auth), and so this server's own version:
// the two are bumped together, so a build says which upstream it follows.
const PluginVersion = "0.1.10"

const (
	// Base is Zen's inference API, where its free models answer: since
	// mid-October 2026 the same request OpenCode sends is answered
	// 403 FreeTierError at /zen/v1 and 200 here.
	Base = "https://opencode.ai/inference/v1"
	// The catalog says what each model is, the docs say what each is asked at.
	catalogURL = "https://models.opencode.ai/api.json"
	docsURL    = "https://raw.githubusercontent.com/anomalyco/opencode/dev/services/www/src/docs/content/console/models.mdx"
)

// ModelRefresh is how often the model list is asked again, as magpie does: a
// model Zen no longer gives free leaves it, and a new free one joins it.
const ModelRefresh = time.Hour

// relistAfter is how long a request for a model the last list didn't have
// waits before it asks Zen's list again: a model Zen made free since is served
// at once, and a model typed in by hand doesn't ask the list every request.
const relistAfter = time.Minute

const discoveryTimeout = 15 * time.Second
