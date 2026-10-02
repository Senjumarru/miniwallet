$ErrorActionPreference = "Stop"
$env:Path += ";$(go env GOPATH)\bin"

function Step($name, [scriptblock]$cmd) {
    Write-Host "== $name ==" -ForegroundColor Cyan
    & $cmd
    if ($LASTEXITCODE -ne 0) { throw "FAILED: $name" }
}

Step "gofmt" {
    $out = gofmt -l .
    if ($out) { Write-Host $out; $global:LASTEXITCODE = 1 }
}
Step "go vet"      { go vet ./... }
Step "go build"    { go build ./... }
Step "mod tidy"    { go mod tidy -diff }
Step "tests -race" { go test -race -count=5 ./... }
Step "govulncheck" { govulncheck ./... }
Step "gosec"       { gosec -quiet ./cmd/server/... ./internal/... }
Step "forbidden patterns" {
    $hits = Get-ChildItem -Recurse -Filter *.go |
        Where-Object { $_.Name -notlike "*_test.go" } |
        Select-String -Pattern "if false &&", "InsecureSkipVerify"
    if ($hits) { $hits | ForEach-Object { Write-Host $_ }; $global:LASTEXITCODE = 1 }
}
Write-Host "ALL CHECKS PASSED" -ForegroundColor Green