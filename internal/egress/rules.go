package egress

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// DefaultRules are the egress rules applied before the admin changes them:
// block Antigravity's Google Clearcut telemetry endpoint.
var DefaultRules = []string{"DOMAIN,play.googleapis.com,REJECT"}

const maxPortSegments = 28

type ruleKind uint8

const (
	ruleDomain ruleKind = iota
	ruleDomainSuffix
	ruleDomainKeyword
	ruleDomainWildcard
	ruleDomainRegex
	ruleIPCIDR
	ruleDstPort
	ruleMatch
)

type actionKind uint8

const (
	actionProxy actionKind = iota
	actionDirect
	actionCustomProxy
	actionReject
	actionRejectDrop
	actionPass
)

type ruleAction struct {
	kind  actionKind
	proxy string // http://host:port for actionCustomProxy
}

type portRange struct {
	start, end uint16
}

// Rule is a compiled mihomo-format egress routing rule.
//
// Examples:
//
//	DOMAIN,play.googleapis.com,REJECT
//	DOMAIN-SUFFIX,googleapis.com,PROXY
//	IP-CIDR,10.0.0.0/8,DIRECT,no-resolve
//	MATCH,PROXY
type Rule struct {
	raw       string
	kind      ruleKind
	text      string // domain, suffix, keyword, or wildcard pattern
	dotSuffix string // "." + text for ruleDomainSuffix
	re        *regexp.Regexp
	prefix    netip.Prefix
	ports     []portRange
	action    ruleAction
}

type targetAddr struct {
	domain string     // lowercased hostname without trailing dots; empty for IP literals
	ip     netip.Addr // normalized IP literal; invalid for hostnames
	port   uint16
}

// ParseRules compiles mihomo-format rule lines. Blank lines and '#' or '//'
// comments are skipped; optional YAML '- ' list prefixes and inline ' #'
// comments are stripped so rules copied from mihomo config.yaml work directly.
func ParseRules(lines []string) ([]Rule, error) {
	var out []Rule
	for i, rawLine := range lines {
		cleaned := cleanRuleLine(rawLine)
		if cleaned == "" {
			continue
		}
		r, err := parseRule(cleaned)
		if err != nil {
			return nil, fmt.Errorf("egress rule %d (%q): %w", i+1, strings.TrimSpace(rawLine), err)
		}
		out = append(out, r)
	}
	return out, nil
}

func cleanRuleLine(line string) string {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
		return ""
	}
	if after, ok := strings.CutPrefix(line, "-"); ok {
		isYAMLItem := after == "" || after[0] == ' ' || after[0] == '\t'
		if isYAMLItem {
			line = strings.TrimSpace(after)
		}
	}
	if idx := inlineCommentIndex(line); idx >= 0 {
		line = strings.TrimSpace(line[:idx])
	}
	line = unquoteRule(line)
	return strings.TrimSpace(line)
}

func inlineCommentIndex(s string) int {
	for i := 1; i < len(s); i++ {
		if s[i] == '#' && (s[i-1] == ' ' || s[i-1] == '\t') {
			return i
		}
	}
	return -1
}

func unquoteRule(s string) string {
	if len(s) < 2 {
		return s
	}
	singleQuoted := s[0] == '\'' && s[len(s)-1] == '\''
	doubleQuoted := s[0] == '"' && s[len(s)-1] == '"'
	if singleQuoted || doubleQuoted {
		return s[1 : len(s)-1]
	}
	return s
}

func parseRule(line string) (Rule, error) {
	tp, payload, target, params, err := splitRuleFields(line)
	if err != nil {
		return Rule{}, err
	}
	act, err := parseAction(target)
	if err != nil {
		return Rule{}, err
	}
	r := Rule{raw: line, action: act}

	switch tp {
	case "DOMAIN":
		domain, err := parseDomainPayload(payload)
		if err != nil {
			return Rule{}, err
		}
		r.kind = ruleDomain
		r.text = domain

	case "DOMAIN-SUFFIX":
		suffix, err := parseDomainPayload(strings.TrimPrefix(payload, "."))
		if err != nil {
			return Rule{}, err
		}
		r.kind = ruleDomainSuffix
		r.text = suffix
		r.dotSuffix = "." + suffix

	case "DOMAIN-KEYWORD":
		keyword := strings.ToLower(payload)
		if keyword == "" {
			return Rule{}, errors.New("empty domain keyword")
		}
		r.kind = ruleDomainKeyword
		r.text = keyword

	case "DOMAIN-WILDCARD":
		pattern := strings.TrimRight(strings.ToLower(payload), ".")
		if pattern == "" {
			return Rule{}, errors.New("empty domain wildcard")
		}
		r.kind = ruleDomainWildcard
		r.text = pattern

	case "DOMAIN-REGEX":
		if payload == "" {
			return Rule{}, errors.New("empty domain regex")
		}
		re, err := regexp.Compile("(?i)" + payload)
		if err != nil {
			return Rule{}, fmt.Errorf("invalid regex: %w", err)
		}
		r.kind = ruleDomainRegex
		r.re = re

	case "IP-CIDR", "IP-CIDR6":
		if err := validateIPParams(params); err != nil {
			return Rule{}, err
		}
		prefix, err := netip.ParsePrefix(payload)
		if err != nil {
			return Rule{}, fmt.Errorf("invalid CIDR %q", payload)
		}
		r.kind = ruleIPCIDR
		r.prefix = prefix.Masked()

	case "DST-PORT":
		ports, err := parsePortRanges(payload)
		if err != nil {
			return Rule{}, err
		}
		r.kind = ruleDstPort
		r.ports = ports

	case "MATCH":
		r.kind = ruleMatch

	default:
		return Rule{}, fmt.Errorf("unsupported rule type %q (supported: DOMAIN, DOMAIN-SUFFIX, DOMAIN-KEYWORD, DOMAIN-WILDCARD, DOMAIN-REGEX, IP-CIDR, IP-CIDR6, DST-PORT, MATCH)", tp)
	}
	return r, nil
}

func splitRuleFields(line string) (tp, payload, target string, params []string, err error) {
	firstComma := strings.IndexByte(line, ',')
	if firstComma < 0 {
		return "", "", "", nil, errors.New("expected TYPE,PAYLOAD,TARGET or MATCH,TARGET")
	}
	tp = strings.ToUpper(strings.TrimSpace(line[:firstComma]))
	rest := line[firstComma+1:]

	if tp == "MATCH" {
		if strings.ContainsRune(rest, ',') {
			return "", "", "", nil, errors.New("MATCH takes only a target: MATCH,TARGET")
		}
		target = strings.TrimSpace(rest)
		if target == "" {
			return "", "", "", nil, errors.New("missing target")
		}
		return tp, "", target, nil, nil
	}

	// Per mihomo, DOMAIN-REGEX patterns may contain commas (e.g. {1,3}) and
	// take no extra parameters, so the target is after the last comma.
	if tp == "DOMAIN-REGEX" {
		lastComma := strings.LastIndexByte(rest, ',')
		if lastComma < 0 {
			return "", "", "", nil, errors.New("expected DOMAIN-REGEX,PATTERN,TARGET")
		}
		payload = strings.TrimSpace(rest[:lastComma])
		target = strings.TrimSpace(rest[lastComma+1:])
		if target == "" {
			return "", "", "", nil, errors.New("missing target")
		}
		return tp, payload, target, nil, nil
	}

	parts := strings.Split(rest, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	if len(parts) < 2 || parts[1] == "" {
		return "", "", "", nil, fmt.Errorf("expected %s,PAYLOAD,TARGET", tp)
	}
	payload, target = parts[0], parts[1]

	hasExtraParams := len(parts) > 2
	allowsParams := tp == "IP-CIDR" || tp == "IP-CIDR6"
	if hasExtraParams && !allowsParams {
		return "", "", "", nil, fmt.Errorf("%s takes no extra parameters", tp)
	}
	if hasExtraParams {
		params = parts[2:]
	}
	return tp, payload, target, params, nil
}

func parseDomainPayload(payload string) (string, error) {
	domain := strings.TrimRight(strings.ToLower(payload), ".")
	if domain == "" {
		return "", errors.New("empty domain")
	}
	if strings.ContainsAny(domain, " \t/:") {
		return "", fmt.Errorf("invalid domain %q", payload)
	}
	if _, err := netip.ParseAddr(domain); err == nil {
		return "", errors.New("use IP-CIDR for IP addresses")
	}
	return domain, nil
}

func validateIPParams(params []string) error {
	for _, p := range params {
		if !strings.EqualFold(p, "no-resolve") {
			return fmt.Errorf("unsupported IP-CIDR parameter %q (only no-resolve is supported)", p)
		}
	}
	return nil
}

func parsePortRanges(raw string) ([]portRange, error) {
	if raw == "" {
		return nil, errors.New("empty port specification")
	}
	segments := strings.Split(raw, "/")
	if len(segments) > maxPortSegments {
		return nil, fmt.Errorf("too many port segments (max %d)", maxPortSegments)
	}
	out := make([]portRange, 0, len(segments))
	for _, seg := range segments {
		pr, err := parsePortSegment(strings.TrimSpace(seg))
		if err != nil {
			return nil, err
		}
		out = append(out, pr)
	}
	return out, nil
}

func parsePortSegment(seg string) (portRange, error) {
	startStr, endStr, isRange := strings.Cut(seg, "-")
	start, err := parsePortNumber(startStr)
	if err != nil {
		return portRange{}, fmt.Errorf("invalid port %q", seg)
	}
	if !isRange {
		return portRange{start: start, end: start}, nil
	}
	end, err := parsePortNumber(endStr)
	if err != nil || start > end {
		return portRange{}, fmt.Errorf("invalid port range %q", seg)
	}
	return portRange{start: start, end: end}, nil
}

func parsePortNumber(s string) (uint16, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 16)
	if err != nil || n == 0 {
		return 0, errors.New("port out of range")
	}
	return uint16(n), nil
}

func parseAction(raw string) (ruleAction, error) {
	switch strings.ToUpper(raw) {
	case "REJECT":
		return ruleAction{kind: actionReject}, nil
	case "REJECT-DROP":
		return ruleAction{kind: actionRejectDrop}, nil
	case "PROXY":
		return ruleAction{kind: actionProxy}, nil
	case "DIRECT":
		return ruleAction{kind: actionDirect}, nil
	case "PASS":
		return ruleAction{kind: actionPass}, nil
	}
	if strings.HasPrefix(strings.ToLower(raw), "http://") {
		proxy, err := cleanProxyURL(raw)
		if err != nil {
			return ruleAction{}, err
		}
		return ruleAction{kind: actionCustomProxy, proxy: proxy}, nil
	}
	return ruleAction{}, fmt.Errorf("unknown target %q (want REJECT, REJECT-DROP, PROXY, DIRECT, PASS, or http://host:port)", raw)
}

func cleanProxyURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	hasHostPort := err == nil && u.Scheme == "http" && u.Hostname() != "" && u.Port() != ""
	hasNoExtras := hasHostPort && (u.Path == "" || u.Path == "/") && u.RawQuery == "" && u.Fragment == ""
	if !hasNoExtras {
		return "", fmt.Errorf("proxy target %q must be http://[user:pass@]host:port", raw)
	}
	return (&url.URL{Scheme: u.Scheme, User: u.User, Host: u.Host}).String(), nil
}

func parseTarget(target string) targetAddr {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		host = strings.Trim(target, "[]")
	}
	var port uint16
	if p, err := strconv.ParseUint(portStr, 10, 16); err == nil {
		port = uint16(p)
	}
	host = strings.TrimRight(strings.ToLower(host), ".")
	if ip, err := netip.ParseAddr(host); err == nil {
		return targetAddr{ip: ip.Unmap().WithZone(""), port: port}
	}
	return targetAddr{domain: host, port: port}
}

func (a targetAddr) hostLabel() string {
	if a.ip.IsValid() {
		return a.ip.String()
	}
	return a.domain
}

func (r Rule) match(addr targetAddr) bool {
	switch r.kind {
	case ruleDomain:
		return addr.domain == r.text
	case ruleDomainSuffix:
		return addr.domain == r.text || strings.HasSuffix(addr.domain, r.dotSuffix)
	case ruleDomainKeyword:
		return addr.domain != "" && strings.Contains(addr.domain, r.text)
	case ruleDomainWildcard:
		return addr.domain != "" && wildcardMatch(r.text, addr.domain)
	case ruleDomainRegex:
		return addr.domain != "" && r.re.MatchString(addr.domain)
	case ruleIPCIDR:
		return addr.ip.IsValid() && r.prefix.Contains(addr.ip)
	case ruleDstPort:
		return addr.port > 0 && matchPort(r.ports, addr.port)
	case ruleMatch:
		return true
	}
	return false
}

func matchPort(ranges []portRange, port uint16) bool {
	for _, pr := range ranges {
		if port >= pr.start && port <= pr.end {
			return true
		}
	}
	return false
}

// wildcardMatch matches s against pattern with mihomo's domain wildcard rules:
// '*' matches zero or more characters, '?' matches one character, '.' is literal.
func wildcardMatch(pattern, s string) bool {
	pi, si := 0, 0
	star, match := -1, 0
	for si < len(s) {
		if pi < len(pattern) && (pattern[pi] == '?' || pattern[pi] == s[si]) {
			pi++
			si++
			continue
		}
		if pi < len(pattern) && pattern[pi] == '*' {
			star = pi
			match = si
			pi++
			continue
		}
		if star >= 0 {
			pi = star + 1
			match++
			si = match
			continue
		}
		return false
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}

type decision struct {
	route  Route
	reject bool
	drop   bool
	rule   string
}

func resolve(rules []Rule, accountRoute Route, target string) decision {
	addr := parseTarget(target)
	for _, r := range rules {
		if !r.match(addr) {
			continue
		}
		switch r.action.kind {
		case actionPass:
			continue
		case actionReject:
			return decision{reject: true, rule: r.raw}
		case actionRejectDrop:
			return decision{reject: true, drop: true, rule: r.raw}
		case actionDirect:
			return decision{route: Route{Direct: true}, rule: r.raw}
		case actionCustomProxy:
			return decision{route: Route{Proxy: r.action.proxy}, rule: r.raw}
		case actionProxy:
			return decision{route: accountRoute, rule: r.raw}
		}
	}
	return decision{route: accountRoute}
}
