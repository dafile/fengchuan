[CmdletBinding()]
param(
    [string[]]$File,
    [string]$Note,
    [string]$NoteName = "update-notes.md",
    [string]$Sender,
    [string]$BaseUrl = $env:FENGCHUAN_URL,
    [string]$ApiKey = $env:FENGCHUAN_API_KEY
)

$ErrorActionPreference = "Stop"
Add-Type -AssemblyName System.Net.Http

if (-not $BaseUrl) {
    $BaseUrl = "http://localhost:8099"
}
if (-not $ApiKey) {
    $keyFile = Join-Path $PSScriptRoot "data/api-key"
    if (Test-Path -LiteralPath $keyFile -PathType Leaf) {
        $ApiKey = ([IO.File]::ReadAllText($keyFile)).Trim()
    }
}
if (-not $ApiKey) {
    throw "Set FENGCHUAN_API_KEY or pass -ApiKey."
}
if (-not $File -and -not $Note) {
    throw "Supply at least one -File or -Note."
}
if (-not $Sender) {
    if ($env:FENGCHUAN_SENDER) {
        $Sender = $env:FENGCHUAN_SENDER
    }
    elseif ($env:CODEX_THREAD_NAME) {
        $Sender = "Codex-" + $env:CODEX_THREAD_NAME
    }
    elseif ($env:CODEX_THREAD_ID) {
        $shortId = ($env:CODEX_THREAD_ID -replace "[^0-9A-Za-z]", "")
        $Sender = "Codex-" + $shortId.Substring(0, [Math]::Min(8, $shortId.Length))
    }
    else {
        $Sender = "Codex"
    }
}

$BaseUrl = $BaseUrl.TrimEnd("/")
$client = [System.Net.Http.HttpClient]::new()
$content = [System.Net.Http.MultipartFormDataContent]::new()

try {
    $client.DefaultRequestHeaders.Authorization =
        [System.Net.Http.Headers.AuthenticationHeaderValue]::new("Bearer", $ApiKey)

    foreach ($entry in @($File)) {
        foreach ($item in ($entry -split "\s*,\s*")) {
            if (-not $item) {
                continue
            }
            $resolved = (Resolve-Path -LiteralPath $item).Path
            if (-not (Test-Path -LiteralPath $resolved -PathType Leaf)) {
                throw "File not found: $item"
            }
            $stream = [System.IO.File]::OpenRead($resolved)
            $part = [System.Net.Http.StreamContent]::new($stream)
            $part.Headers.ContentType =
                [System.Net.Http.Headers.MediaTypeHeaderValue]::new("application/octet-stream")
            $content.Add($part, "file", [System.IO.Path]::GetFileName($resolved))
        }
    }

    $content.Add(
        [System.Net.Http.StringContent]::new($Sender, [System.Text.Encoding]::UTF8),
        "sender"
    )
    if ($Note) {
        $content.Add(
            [System.Net.Http.StringContent]::new($Note, [System.Text.Encoding]::UTF8, "text/markdown"),
            "note"
        )
        $content.Add(
            [System.Net.Http.StringContent]::new($NoteName, [System.Text.Encoding]::UTF8),
            "note_name"
        )
    }

    $response = $client.PostAsync("$BaseUrl/api/upload", $content).GetAwaiter().GetResult()
    $body = $response.Content.ReadAsStringAsync().GetAwaiter().GetResult()
    if (-not $response.IsSuccessStatusCode) {
        throw "Upload failed ($([int]$response.StatusCode)): $body"
    }
    $body | ConvertFrom-Json | ConvertTo-Json -Depth 8
}
finally {
    $content.Dispose()
    $client.Dispose()
}
