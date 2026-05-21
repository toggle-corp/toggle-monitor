package slack

import "strings"

// ResolveMentions turns a notify list (slugs and/or raw Slack markup)
// into resolved markup strings ready to drop into a Block Kit section.
//
//   - Slug present in userMapping → emit `<@U…>` for user IDs (U-prefix)
//     or `<!subteam^S…>` for subteams (S-prefix).
//   - Raw `<…>` markup → pass through verbatim.
//   - Anything else → dropped (config validation should have caught it).
//
// Union dedup: identical resolved markup strings appear once even if
// the same user is mentioned by both a monitor-level and a group-level
// notify entry.
func ResolveMentions(notify []string, userMapping map[string]string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(notify))
	for _, raw := range notify {
		var resolved string
		if id, ok := userMapping[raw]; ok {
			if strings.HasPrefix(id, "U") {
				resolved = "<@" + id + ">"
			} else if strings.HasPrefix(id, "S") {
				resolved = "<!subteam^" + id + ">"
			} else {
				continue
			}
		} else if isRawMarkup(raw) {
			resolved = raw
		} else {
			continue
		}
		if _, dup := seen[resolved]; dup {
			continue
		}
		seen[resolved] = struct{}{}
		out = append(out, resolved)
	}
	return out
}

func isRawMarkup(s string) bool {
	return len(s) >= 2 && s[0] == '<' && s[len(s)-1] == '>'
}
