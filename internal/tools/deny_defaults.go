package tools

// DefaultDenyPOSIX is the starting tools.exec.deny list `onboard` writes for
// a bash/zsh/sh default shell. It is necessary, not sufficient: a deny-list
// stops accidents and naive prompt injection, not a determined attacker who
// already has message access. Do not "simplify" these patterns without
// re-running the corpus test in policy_test.go: both `rm --recursive
// --force /` and `/bin/rm -rf /` must keep matching the rm rules below, and
// TestDenyCorpus_RmRulesCatchLongFlagsAndPathPrefixedForms pins exactly
// that.
var DefaultDenyPOSIX = []string{
	// recursive/forced rm - short flags (either case: -r and -R both mean
	// recursive), long flags, path-prefixed invocations, and a command
	// separator, open paren or backtick directly before rm (`ls;rm -rf ~`,
	// `(rm -rf ~) &`, "echo `rm -rf ~`"), none of which is whitespace and
	// would otherwise slip past the boundary group below.
	`(^|[;&|(\x60]|\s)(/\S*/)?rm\s+([^|;&]*\s)?-[a-zA-Z]*[rRf]`,
	`(^|[;&|(\x60]|\s)(/\S*/)?rm\s+([^|;&]*\s)?--(recursive|force)\b`,
	`\bfind\b[^|;&]*\s-delete\b`,
	`\bmkfs(\.|\s)`,
	`\bdd\s+.*\bof=/dev/`,
	`:\(\)\s*\{.*\};\s*:`, // fork bomb
	`\b(shutdown|reboot|halt|poweroff)\b`,
	`>\s*/dev/(sd|nvme|disk)`,
	`\bchmod\b[^|;&]*\b777\s+(-[a-zA-Z]+\s+)*/`,
	`\b(curl|wget)\b.*\|\s*(sudo\s+)?(/\S*/)?(ba|z|da)?sh`, // pipe-to-shell
	gitForcePushPattern,
	`\b(userdel|groupdel|passwd)\b`,
	`\bhistory\s+-c\b`,
}

// gitForcePushPattern matches a force push in any spelling: --force,
// --force-with-lease, a short-flag cluster containing f (-f, -fu), and a
// "+refspec" (`git push origin +main`). Global options between git and push
// (`git -C . push -f`) are allowed for; anything else between the two words
// means "push" is an argument to some other subcommand
// (`git commit -m 'push +1'`), not the push command.
const gitForcePushPattern = `\bgit\b(\s+-[cC]\s+\S+|\s+-\S+)*\s+push\b[^|;&]*(\s--force(-with-lease)?\b|\s-[a-zA-Z]*f|\s\+\S)`

// DefaultDenyWindows is the starting tools.exec.deny list `onboard` writes
// when the default shell is PowerShell. PowerShell is case-insensitive, so
// every pattern carries (?i). The delete rule targets PowerShell itself:
// rm/ri/del/erase/rd/rmdir are all aliases of Remove-Item, and parameter
// names may be abbreviated (-r, -Rec, -fo), so any -r* or -fo* parameter on
// one of them is caught. See DefaultDenyPOSIX's comment for the same
// "do not simplify without re-testing" warning.
var DefaultDenyWindows = []string{
	`(?i)(^|[;&|(\s])(remove-item|ri|rm|rmdir|rd|del|erase)(\s[^|;&]*)?\s-(r|fo)[a-z]*\b`,
	`(?i)\b(rd|rmdir)\b[^|;&]*\s/s\b`,
	`(?i)\bdel\b[^|;&]*\s/[fsq]\b`,
	`(?i)\b(format-volume|clear-disk|initialize-disk|diskpart)\b`,
	`(?i)\b(stop-computer|restart-computer|shutdown)\b`,
	`(?i)\breg\s+delete\b`,
	`(?i)\bset-executionpolicy\b`,
	`(?i)\b(iwr|irm|invoke-webrequest|invoke-restmethod|curl|wget)\b.*\|\s*(iex|invoke-expression)\b`,
	`(?i)\b(iex|invoke-expression)\b\s*\(\s*(iwr|irm|invoke-webrequest|invoke-restmethod)\b`,
	`(?i)` + gitForcePushPattern,
}
