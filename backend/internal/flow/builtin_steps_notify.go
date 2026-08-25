// Discord notification step. Posts a message to a Discord webhook when the
// flow reaches the step — used to announce episode progress ("encode
// finished for X") without waiting for the controller's job-outcome alert.
// Best-effort by design: an unreachable webhook warns and the job continues.
package flow

import "github.com/badskater/encode-system/backend/internal/model"

func discordNotifyTemplate() *model.StepTemplate {
	return &model.StepTemplate{
		Key:         "discord_notify",
		Label:       "Discord notification",
		Description: "Post a message to a Discord webhook when the flow reaches this step (e.g. after mux or release copy). Webhook comes from the param, falling back to the controller's configured webhook. Best-effort: an unreachable webhook warns instead of failing the job.",
		Builtin:     true,
		Params: []model.ParamDef{
			{Key: "webhook", Label: "Webhook URL (blank = controller's configured webhook)"},
			{Key: "message", Label: "Extra message line (blank = standard episode summary)"},
		},
		PowerShell: `function Invoke-DiscordNotify {
    param(
        [Parameter(Mandatory=$true)] [pscustomobject] $Job,
        [pscustomobject] $Params
    )
    Write-Output "ENCODE_STEP discord_notify 0"

    # Webhook resolution: flow param first, then the controller-wide
    # webhook injected into the $Job context. No webhook anywhere = no-op
    # with a warning (never fail an encode over a missing notification).
    $webhook = ''
    if ($Params.webhook) { $webhook = $Params.webhook.Trim() }
    if ($webhook -eq '' -and $Job.PSObject.Properties['DiscordWebhook'] -and $Job.DiscordWebhook) {
        $webhook = $Job.DiscordWebhook.Trim()
    }
    if ($webhook -eq '') {
        Write-Output "[discord] no webhook configured (param blank, controller webhook empty) — skipping"
        Write-Output "ENCODE_STEP discord_notify 100"
        return
    }
    # URL guard: only real Discord webhook hosts (plus loopback for local
    # mock-webhook testing) — an arbitrary URL here would exfiltrate job
    # details to any host the param points at.
    if ($webhook -notmatch '^https://(discord|discordapp)\.com/api/webhooks/' -and
        $webhook -notmatch '^https?://(localhost|127\.0\.0\.1)(:\d+)?/') {
        Write-Output "[discord] webhook does not look like a Discord webhook URL — skipping"
        Write-Output "ENCODE_STEP discord_notify 100"
        return
    }

    $extra = ''
    if ($Params.message) { $extra = $Params.message.Trim() }
    $node = $env:COMPUTERNAME
    $lines = @("📢 **Encode progress** — $($Job.Series) Ep $($Job.Episode)")
    # Discord inline-code markers are literal backticks; built via [char]96
    # so this template stays free of raw backticks (Go raw-string limit).
    $bt = [char]96
    $lines += "node: $bt$node$bt · release: $bt$($Job.OutputName)$bt"
    if ($extra -ne '') { $lines += $extra }
    $content = $lines -join [Environment]::NewLine
    # Discord caps content at 2000 characters; leave room instead of 400ing.
    if ($content.Length -gt 1900) { $content = $content.Substring(0, 1900) + '…' }

    # Explicit UTF-8 body: series names are unicode (Japanese titles) and
    # PS 5.1's default body encoding would mojibake them. WebClient is used
    # deliberately — it behaves identically on PS 5.1 nodes and pwsh.
    $payload = @{ content = $content } | ConvertTo-Json -Compress
    try {
        $wc = New-Object System.Net.WebClient
        $wc.Encoding = [System.Text.Encoding]::UTF8
        $wc.Headers.Add('Content-Type', 'application/json; charset=utf-8')
        $resp = $wc.UploadString($webhook, 'POST', $payload)
        Write-Output "[discord] message posted (response: $($resp.Length) chars)"
    } catch {
        # Best-effort: a webhook outage must never take down the encode.
        Write-Output "[discord] WARNING: notification failed ($($_.Exception.Message)) — continuing"
    }
    Write-Output "ENCODE_STEP discord_notify 100"
}`,
	}
}
