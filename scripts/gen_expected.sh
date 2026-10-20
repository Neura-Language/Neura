#!/usr/bin/env bash
# 生成 testdata/expected_errors.json（§17 阶段 B 回填）。
#
# 数据来源 = parser/checker 的实际输出（--emit-expected），不是人工数。
# CI 用法：重新生成后与版本库里的文件比对，有差异就说明 span 变了，必须人工确认后再提交。
set -euo pipefail
cd "$(dirname "$0")/.."

go build -o neura-bin ./cmd/neura
./neura-bin --emit-expected testdata/neg/N*.neura > testdata/expected_errors.json
echo "written testdata/expected_errors.json ($(grep -c '"file"' testdata/expected_errors.json) files)"
