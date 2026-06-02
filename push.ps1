param(
    [string]$Message = "update"
)

$ErrorActionPreference = "Stop"

git add .

$staged = git diff --cached --name-only
if (-not $staged) {
    Write-Host "No changes to commit."
    exit 0
}

git commit -m $Message
if ($LASTEXITCODE -ne 0) {
    exit $LASTEXITCODE
}

git push
if ($LASTEXITCODE -ne 0) {
    exit $LASTEXITCODE
}
