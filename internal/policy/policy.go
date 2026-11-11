// Package policy classifies shell commands an agent wants to run.
//
//   - Fatal commands are always denied, even in autonomous mode.
//   - Safe commands (read-only) are allowed without asking.
//   - Everything else needs a human: in the terminal or on the phone.
//
// A command is only safe when *every* segment of a pipeline/chain is safe and
// it does not write files or spawn sub-shells. When in doubt, ask.
package policy

import (
	"fmt"
	"regexp"
	"strings"
)

type Verdict int

const (
	Ask Verdict = iota
	Allow
	Deny
)

func (v Verdict) String() string {
	switch v {
	case Allow:
		return "allow"
	case Deny:
		return "deny"
	}
	return "ask"
}

type Policy struct {
	safe  []*regexp.Regexp
	fatal []*regexp.Regexp
}

// Default read-only commands. Anything that can execute arbitrary code
// (python, node, bash -c, curl | sh, git commit, …) is deliberately absent.
var defaultSafe = []string{
	`^(ls|ll|la|tree|pwd|cat|bat|head|tail|less|more|wc|nl|sort|uniq|cut|tr|column|diff|cmp|file|stat|du|df|free|uptime|uname|whoami|id|date|which|type|echo|printf|true|realpath|basename|dirname|readlink)(\s|$)`,
	`^(grep|egrep|fgrep|rg|ag|fd|jq|yq)(\s|$)`,
	`^find(\s|$)`,
	`^git(\s+-C\s+\S+)?\s+(status|diff|log|show|branch|rev-parse|describe|blame|shortlog|ls-files|remote(\s+-v)?|tag(\s+(-l|--list))?|stash\s+list|config\s+(--get|-l|--list))(\s|$)`,
	`^(go\s+(version|env|list|vet|doc)|cargo\s+(tree|metadata)|npm\s+(ls|view|outdated)|pnpm\s+(ls|why|outdated)|bun\s+pm\s+ls)(\s|$)`,
	`^(systemctl\s+(--user\s+)?(status|is-active|is-enabled|list-units)|journalctl)(\s|$)`,
	`^(docker|podman)\s+(ps|images|logs|inspect|stats\s+--no-stream)(\s|$)`,
	`^kubectl\s+(get|describe|logs|top)(\s|$)`,
	`^agyh(\s|$)`,
}

var defaultFatal = []string{
	`\bsudo\b`,
	`\brm\s+(-\w*\s+)*-\w*[rR]\w*\s+.*(\s|^)(/|~|\$HOME|\.\.|\*|/\*)(\s|$)`,
	`\brm\s+(-\w*\s+)*-\w*[rR]\w*\s+(/|~|\$HOME)(\s|$)`,
	`\bgit\s+push\b.*(\s--force(-with-lease)?\b|\s-f\b)`,
	`\bgit\s+(reset\s+--hard|clean\s+-\w*f)`,
	`\b(mkfs(\.\w+)?|fdisk|parted|wipefs)\b`,
	`\bdd\s+.*\bof=/dev/`,
	`>\s*/dev/(sd[a-z]|nvme\d|disk\d)`,
	`\bchmod\s+(-\w+\s+)*(777|000)\s+(/|~)`,
	`(/etc/shadow|/etc/sudoers|\.ssh/authorized_keys|\.ssh/id_[a-z0-9]+)`,
	`\b(curl|wget)\b[^|]*\|\s*(sudo\s+)?(ba|z|da)?sh\b`,
	`:\(\)\s*\{\s*:\|:&\s*\};:`, // fork bomb
	`\b(shutdown|reboot|halt|poweroff)\b`,
}

// New builds a policy from the defaults plus user-supplied extra patterns.
func New(extraSafe, extraFatal []string) (*Policy, error) {
	p := &Policy{}
	for _, s := range append(append([]string{}, defaultSafe...), extraSafe...) {
		re, err := regexp.Compile("(?i)" + s)
		if err != nil {
			return nil, fmt.Errorf("safe pattern %q: %w", s, err)
		}
		p.safe = append(p.safe, re)
	}
	for _, s := range append(append([]string{}, defaultFatal...), extraFatal...) {
		re, err := regexp.Compile("(?i)" + s)
		if err != nil {
			return nil, fmt.Errorf("fatal pattern %q: %w", s, err)
		}
		p.fatal = append(p.fatal, re)
	}
	return p, nil
}

// MustDefault returns the built-in policy.
func MustDefault() *Policy {
	p, err := New(nil, nil)
	if err != nil {
		panic(err)
	}
	return p
}

// Classify returns the verdict for a shell command and a short reason.
func (p *Policy) Classify(cmd string) (Verdict, string) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return Ask, "empty command"
	}
	for _, re := range p.fatal {
		if re.MatchString(cmd) {
			return Deny, "blocked by policy: " + re.String()[4:]
		}
	}
	if reason := unsafeShell(cmd); reason != "" {
		return Ask, reason
	}
	for _, seg := range splitChain(devNull.ReplaceAllString(cmd, "")) {
		if !p.isSafe(seg) {
			return Ask, "needs approval: " + firstWord(seg)
		}
	}
	return Allow, "read-only command"
}

// IsFatal reports whether cmd matches a fatal pattern.
func (p *Policy) IsFatal(cmd string) bool {
	v, _ := p.Classify(cmd)
	return v == Deny
}

func (p *Policy) isSafe(seg string) bool {
	for _, re := range p.safe {
		if re.MatchString(seg) {
			return true
		}
	}
	return false
}

var (
	devNull  = regexp.MustCompile(`\d?>>?\s*/dev/null|\d?>&\d`)
	findExec = regexp.MustCompile(`(?i)\bfind\b.*\s-(exec|execdir|ok|okdir|delete|fprint\w*)\b`)
)

// unsafeShell flags constructs that make an otherwise read-only command
// dangerous: output redirection, command substitution, find -exec/-delete.
func unsafeShell(cmd string) string {
	clean := devNull.ReplaceAllString(cmd, "")
	switch {
	case strings.ContainsAny(clean, "><"):
		return "redirects input/output"
	case strings.Contains(clean, "$(") || strings.Contains(clean, "`"):
		return "uses command substitution"
	case findExec.MatchString(clean):
		return "find with -exec/-delete"
	}
	return ""
}

// splitChain splits on |, ||, &&, ; and & outside of quotes.
func splitChain(cmd string) []string {
	var (
		out   []string
		cur   strings.Builder
		quote rune
	)
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}
	rs := []rune(cmd)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else if r == '\\' && quote == '"' && i+1 < len(rs) {
				cur.WriteRune(r)
				i++
				r = rs[i]
			}
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
			cur.WriteRune(r)
		case r == '|' || r == ';' || r == '&' || r == '\n':
			flush()
			if i+1 < len(rs) && (rs[i+1] == '|' || rs[i+1] == '&') {
				i++
			}
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

func firstWord(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return s
}
