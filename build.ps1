$ErrorActionPreference = "Stop"
New-Item -ItemType Directory -Force dist | Out-Null

function Resolve-CompilerCommand {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Compiler
    )

    $trimmed = $Compiler.Trim()
    if (-not $trimmed) {
        return $null
    }

    $first = $trimmed[0]
    if ($first -eq '"' -or $first -eq "'") {
        $closing = $trimmed.IndexOf($first, 1)
        if ($closing -lt 1) {
            throw "go env CC starts with a quote but does not contain a closing quote: $Compiler"
        }

        $quotedCommand = $trimmed.Substring(1, $closing - 1).Trim()
        if (-not $quotedCommand) {
            throw "go env CC contains an empty quoted compiler command: $Compiler"
        }
        return $quotedCommand
    }

    if (Get-Command $trimmed -ErrorAction SilentlyContinue) {
        return $trimmed
    }

    $parts = $trimmed -split "\s+"
    for ($i = $parts.Count; $i -gt 0; $i--) {
        $candidate = ($parts[0..($i - 1)] -join " ")
        if (Get-Command $candidate -ErrorAction SilentlyContinue) {
            return $candidate
        }
    }

    return $parts[0]
}

$env:CGO_ENABLED = "1"
$cc = (go env CC).Trim()
if (-not $cc) {
    throw "A C compiler is required to build the CPA dynamic plugin DLL (-buildmode=c-shared), but go env CC is empty. Install a C toolchain such as MinGW-w64 and ensure it is on PATH."
}

$ccCommand = Resolve-CompilerCommand $cc
if (-not (Get-Command $ccCommand -ErrorAction SilentlyContinue)) {
    throw "A C compiler is required to build the CPA dynamic plugin DLL (-buildmode=c-shared), but the compiler from go env CC ('$cc') could not be found (resolved command: '$ccCommand'). Install a C toolchain such as MinGW-w64 and ensure it is on PATH."
}

go test ./...
go build -buildmode=c-shared -o dist/codex-auto-reset.dll .
