#!/usr/bin/env bash
set -euo pipefail

kernel_third_party_modules=$(go list -deps -f \
  '{{with .Module}}{{if not .Main}}{{.Path}}{{end}}{{end}}' \
  ./agent ./llm ./store ./tool | sed '/^$/d' | sort -u)
test -z "$kernel_third_party_modules"

sdk_root_requirement=$(go list -m -f \
  '{{.Path}} {{.Version}} {{if .Indirect}}indirect{{else}}direct{{end}}' \
  github.com/modelcontextprotocol/go-sdk)
test "$sdk_root_requirement" = 'github.com/modelcontextprotocol/go-sdk v1.7.0 direct'

unexpected_sdk_importers=$(go list -test -f \
  '{{.ImportPath}}{{printf "\t"}}{{if .ForTest}}{{.ForTest}}{{else}}-{{end}}{{printf "\t"}}{{join .Imports " "}} {{join .TestImports " "}} {{join .XTestImports " "}}' \
  ./... | awk -F '\t' \
  -v sdk='github.com/modelcontextprotocol/go-sdk' \
  -v allowed='github.com/dailz1/go-agent/mcp' '
    {
      id = ($2 != "-" ? $2 : $1)
      n = split($3, imports, " ")
      for (i = 1; i <= n; i++) {
        if (imports[i] == sdk || index(imports[i], sdk "/") == 1) {
          if (id != allowed && index(id, allowed "/") != 1) print $1
          break
        }
      }
    }
  ' | sort -u)
test -z "$unexpected_sdk_importers"
