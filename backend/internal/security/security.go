// Package security flags agent steps that look like a security problem:
// instructions planted in content the agent read (prompt injection), a
// credential surfacing in a result, or data leaving through a tool call.
//
// Flags are signals for a person to review, not verdicts. The detectors are
// deliberately conservative (specific phrasings, long thresholds) and cheap
// (a few RE2 scans over text that is already in memory). They run on
// redacted text, so they never see or store a secret: a credential is known
// only by the redaction marker that replaced it.
package security

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Flags stored on a step (security_flags).
const (
	PromptInjection = "prompt_injection"
	SecretExposed   = "secret_exposed"
	Exfiltration    = "possible_exfiltration"
)

// Step types the detectors look at (model.Step types).
const (
	typeLLM       = "llm_call"
	typeTool      = "tool_call"
	typeRetrieval = "retrieval"
)

// maxScan bounds the text scanned per field; injected instructions and
// credentials sit well inside it in practice.
const maxScan = 256 * 1024

// Result is what Detect found on one step.
type Result struct {
	Flags   []string // sorted, unique
	Reasons []string // "flag:reason", sorted, unique
	Secrets int      // credential redaction markers in the step's output
}

// Empty reports whether nothing was found.
func (r Result) Empty() bool { return len(r.Flags) == 0 }

type rule struct {
	reason string
	re     *regexp.Regexp
}

// Injection phrasings. Each is specific enough that ordinary tool results
// ("ignore this email", "you are now subscribed", "the system prompt
// field") don't match.
var injectionRules = []rule{
	{"ignore_instructions", regexp.MustCompile(`(?i)\b(?:ignore|disregard|forget|override)\s+(?:all\s+|any\s+|the\s+|your\s+|of\s+)*(?:previous|prior|above|earlier|preceding|original|system)\s+(?:instructions?|prompts?|directions?|rules|guidelines)\b`)},
	{"reveal_system_prompt", regexp.MustCompile(`(?i)\b(?:reveal|print|show|output|repeat|disclose|leak)\s+(?:me\s+)?(?:your|the)\s+(?:full\s+|entire\s+|original\s+|hidden\s+)?(?:system\s+prompt|system\s+message|hidden\s+instructions|initial\s+instructions)\b`)},
	{"conceal_from_user", regexp.MustCompile(`(?i)\b(?:do\s+not|don't|never)\s+(?:tell|inform|notify|alert)\s+the\s+user\b|\bwithout\s+(?:telling|informing|notifying)\s+the\s+user\b`)},
	{"role_override", regexp.MustCompile(`(?i)\byou\s+are\s+now\s+(?:DAN\b|in\s+(?:developer|god|jailbreak|unrestricted)\s+mode\b|(?:an?\s+)?(?:unrestricted|unfiltered|jailbroken)\b|no\s+longer\s+(?:bound|restricted)\b)`)},
	{"chat_markup", regexp.MustCompile(`<\|im_start\|>|<\|im_end\|>|<\|system\|>|\[/?INST\]|<<SYS>>|</?system>`)},
}

var (
	credentialMarker = regexp.MustCompile(`\[REDACTED:(?:SECRET|PRIVATE_KEY)\]`)
	// Markers for data that should not leave through an outbound tool.
	// Email addresses and phone numbers are left out: sending an email
	// naturally carries an address.
	sensitiveMarker = regexp.MustCompile(`\[REDACTED:(?:SECRET|PRIVATE_KEY|CARD|SSN)\]`)
	urlRe           = regexp.MustCompile(`https?://[^\s"'<>\\]+`)
	blobRe          = regexp.MustCompile(`[A-Za-z0-9+/_-]{200,}={0,2}`)
	outboundToolRe  = regexp.MustCompile(`(?i)(?:e-?mail|mail|send|post|http|request|fetch|webhook|upload|slack|message|sms|curl|browse|navigate|publish)`)
)

const (
	minQueryLen      = 256 // a URL query string this long can carry a document
	minQueryValueLen = 128
	minTagChars      = 4 // invisible Unicode tag characters (ASCII smuggling)
)

// Detect inspects one normalized step. input and output must already be
// redacted; toolName is the tool's name for tool steps.
func Detect(stepType, toolName, input, output string) Result {
	input, output = clip(input), clip(output)
	found := map[string]bool{}
	var res Result
	add := func(flag, reason string) {
		found[flag+":"+reason] = true
	}

	// Prompt injection: instructions inside content the agent read.
	if stepType == typeTool || stepType == typeRetrieval {
		for _, r := range injectionRules {
			if r.re.MatchString(output) {
				add(PromptInjection, r.reason)
			}
		}
		if tagChars(output) >= minTagChars {
			add(PromptInjection, "hidden_text")
		}
	}

	// A credential in a result: the redactor replaced it before storage.
	if stepType == typeTool || stepType == typeRetrieval || stepType == typeLLM {
		if n := len(credentialMarker.FindAllStringIndex(output, -1)); n > 0 {
			res.Secrets = n
			add(SecretExposed, "credential_in_output")
		}
	}

	// Data leaving through a tool call's arguments.
	if stepType == typeTool {
		if longQuery(input) {
			add(Exfiltration, "long_url_query")
		}
		if encodedBlob(input) {
			add(Exfiltration, "encoded_blob")
		}
		if outboundToolRe.MatchString(toolName) && sensitiveMarker.MatchString(input) {
			add(Exfiltration, "sensitive_data_to_outbound_tool")
		}
	}

	if len(found) == 0 {
		return res
	}
	flags := map[string]bool{}
	for k := range found {
		res.Reasons = append(res.Reasons, k)
		flags[k[:strings.IndexByte(k, ':')]] = true
	}
	for f := range flags {
		res.Flags = append(res.Flags, f)
	}
	sort.Strings(res.Reasons)
	sort.Strings(res.Flags)
	return res
}

func clip(s string) string {
	if len(s) > maxScan {
		return s[:maxScan]
	}
	return s
}

// tagChars counts Unicode tag characters (U+E0000-U+E007F): invisible in
// most renderers but read by models, the "ASCII smuggling" technique.
func tagChars(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0xE0000 && r <= 0xE007F {
			n++
		}
	}
	return n
}

func longQuery(s string) bool {
	for _, raw := range urlRe.FindAllString(s, 20) {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		if len(u.RawQuery) >= minQueryLen {
			return true
		}
		for _, vs := range u.Query() {
			for _, v := range vs {
				if len(v) >= minQueryValueLen {
					return true
				}
			}
		}
	}
	return false
}

// encodedBlob finds a long run of base64-alphabet characters mixing upper
// case, lower case and digits (an encoded document, not a long word,
// path or hash).
func encodedBlob(s string) bool {
	for _, m := range blobRe.FindAllString(s, 10) {
		var up, lo, dig bool
		for _, r := range m {
			switch {
			case unicode.IsUpper(r):
				up = true
			case unicode.IsLower(r):
				lo = true
			case unicode.IsDigit(r):
				dig = true
			}
		}
		if up && lo && dig {
			return true
		}
	}
	return false
}

// Explain is the plain-language meaning of each reason, for alerts.
var Explain = map[string]string{
	"prompt_injection:ignore_instructions":                  "content the agent read tells it to ignore its instructions",
	"prompt_injection:reveal_system_prompt":                 "content the agent read asks it to reveal its system prompt",
	"prompt_injection:conceal_from_user":                    "content the agent read tells it to hide something from the user",
	"prompt_injection:role_override":                        "content the agent read tries to give it a new, unrestricted role",
	"prompt_injection:chat_markup":                          "content the agent read contains chat-format control tokens",
	"prompt_injection:hidden_text":                          "content the agent read contains invisible text",
	"secret_exposed:credential_in_output":                   "a credential appeared in a result (redacted before storage)",
	"possible_exfiltration:long_url_query":                  "a tool call sent a URL with a very long query string",
	"possible_exfiltration:encoded_blob":                    "a tool call's arguments carry a long encoded blob",
	"possible_exfiltration:sensitive_data_to_outbound_tool": "sensitive data was passed to a tool that sends data out",
}
