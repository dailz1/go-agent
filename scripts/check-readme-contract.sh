#!/usr/bin/env bash
set -euo pipefail

readonly module_path=$(awk '$1 == "module" { print $2; exit }' go.mod)
readonly go_version=$(awk '$1 == "go" { print $2; exit }' go.mod)
test -n "$module_path"
test -n "$go_version"

check_readme() {
	local file=$1
	test -f "$file"
	grep -Fq "$module_path" "$file"
	grep -Fq "Go $go_version" "$file"
	grep -Fq 'agenttool.New' "$file"
	grep -Fq 'mcpbridge.Connect' "$file"
	grep -Fqx 'git clone https://github.com/dailz1/go-agent.git' "$file"
	grep -Fqx 'cd go-agent' "$file"
	grep -Fqx "export OPENAI_API_KEY='...'" "$file"
	grep -Fqx '# OPENAI_BASE_URL and OPENAI_MODEL are optional.' "$file"
	grep -Fqx "go get $module_path@latest" "$file"
	grep -Fqx 'go run ./examples/quickstart' "$file"

	local match target
	while IFS= read -r match; do
		target=${match:2}
		target=${target%%#*}
		target=${target%%\?*}
		case $target in
			""|http://*|https://*|mailto:*) continue ;;
		esac
		test -e "$target"
	done < <(grep -oE '\]\([^ )]+' "$file")
}

check_readme README.md
check_readme README.zh-CN.md
