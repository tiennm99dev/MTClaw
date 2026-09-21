package tools

// DefaultDenyPOSIX is the starting tools.exec.deny list `onboard` writes for
// a bash/zsh/sh default shell. It is necessary, not sufficient: a deny-list
// stops accidents and naive prompt injection, not a determined attacker who
// already has message access. Patterns are copied verbatim from
// plans/260731-2219-mtclaw-core-system/phase-05-tools-and-policy-engine.md;
// do not "simplify" them without re-running the corpus test in
// policy_test.go, because two earlier, more obvious versions of the rm
// patterns were bypassed by `rm --recursive --force /` and `/bin/rm -rf /`
// respectively.
var DefaultDenyPOSIX = []string{
	// recursive/forced rm - short flags, long flags, path-prefixed
	// invocations, and a subshell/group open paren directly before rm
	// (`(rm -rf ~) &`), which is not whitespace and would otherwise slip
	// past the boundary group below.
	`(^|[;&|]\s|\s|\()(/\S*/)?rm\s+([^|;&]*\s)?-[a-zA-Z]*[rf]`,
	`(^|[;&|]\s|\s|\()(/\S*/)?rm\s+([^|;&]*\s)?--(recursive|force)\b`,
	`\bfind\b[^|;&]*\s-delete\b`,
	`\bmkfs(\.|\s)`,
	`\bdd\s+.*\bof=/dev/`,
	`:\(\)\s*\{.*\};\s*:`, // fork bomb
	`\b(shutdown|reboot|halt|poweroff)\b`,
	`>\s*/dev/(sd|nvme|disk)`,
	`\bchmod\s+(-[a-zA-Z]+\s+)*(-R\s+)?777\s+/`,
	`\b(curl|wget)\b.*\|\s*(sudo\s+)?(ba|z|da)?sh`, // pipe-to-shell
	`\bgit\s+push\b.*(--force(-with-lease)?|-f)\b`,
	`\b(userdel|groupdel|passwd)\b`,
	`\bhistory\s+-c\b`,
}

// DefaultDenyWindows is the starting tools.exec.deny list `onboard` writes
// when the default shell is PowerShell. PowerShell is case-insensitive, so
// every pattern carries (?i). Copied verbatim from the phase 5 plan file;
// see DefaultDenyPOSIX's comment for the same "do not simplify without
// re-testing" warning.
var DefaultDenyWindows = []string{
	`(?i)\bremove-item\b[^|;&]*\s-(recurse|force)\b`,
	`(?i)\b(rd|rmdir)\b[^|;&]*\s/s\b`,
	`(?i)\bdel\b[^|;&]*\s/[fsq]\b`,
	`(?i)\b(format-volume|clear-disk|initialize-disk|diskpart)\b`,
	`(?i)\b(stop-computer|restart-computer|shutdown)\b`,
	`(?i)\breg\s+delete\b`,
	`(?i)\bset-executionpolicy\b`,
	`(?i)\b(iwr|invoke-webrequest|curl|wget)\b.*\|\s*(iex|invoke-expression)\b`,
	`(?i)\b(iex|invoke-expression)\b\s*\(\s*(iwr|invoke-webrequest)\b`,
	`(?i)\bgit\s+push\b.*(--force(-with-lease)?|-f)\b`,
}
