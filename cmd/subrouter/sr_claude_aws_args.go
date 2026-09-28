package main

import (
	"fmt"
	"strings"
)

type claudeAWSArgs struct {
	account, model, region string
	passthrough            []string
}

// A named shorthand uses the same launcher and parsing as the canonical AWS
// command. A pin cannot be replaced by a later flag. Preserve Claude arguments
// and their order; -- ends wrapper parsing and is forwarded as well.
func parseClaudeAWSArgs(args []string, pinnedAccount string) (claudeAWSArgs, error) {
	out := claudeAWSArgs{account: pinnedAccount, model: "fable", region: "us-east-1"}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			out.passthrough = append(out.passthrough, args[i:]...)
			break
		}
		name, value, inline := strings.Cut(arg, "=")
		switch name {
		case "--account", "--aws-account", "--model", "-m", "--aws-region":
			if !inline {
				if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
					return out, fmt.Errorf("%s requires a value", name)
				}
				i++
				value = args[i]
			}
			value = strings.TrimSpace(value)
			if value == "" {
				return out, fmt.Errorf("%s requires a nonempty value", name)
			}
			switch name {
			case "--account", "--aws-account":
				for _, c := range value {
					if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.') {
						return out, fmt.Errorf("invalid AWS account label")
					}
				}
				if pinnedAccount != "" && value != pinnedAccount {
					return out, fmt.Errorf("this command is pinned to AWS account %q; use sr claude-aws --account for a different account", pinnedAccount)
				}
				if out.account != "" && out.account != value {
					return out, fmt.Errorf("conflicting AWS account selectors")
				}
				out.account = value
			case "--aws-region":
				out.region = value
			default:
				out.model = value
			}
		default:
			out.passthrough = append(out.passthrough, arg)
		}
	}
	return out, nil
}
