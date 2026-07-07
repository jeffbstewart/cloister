# Runtime drift probe for a deployed cell's scholar containment.  Pair with
# the static compose-lint (commit-time CI) and the scholar's own fail-closed
# boot self-check.  Cron this against a live cell.
#
#   .\probe-scholar-egress.ps1 -Project myproject
#
# Exits 0 if contained, 1 on any violation.  Read-only except for throwaway
# alpine containers it runs (--rm) to test the scholar's network position.
param([Parameter(Mandatory = $true)][string]$Project)

$ErrorActionPreference = 'Stop'
$fail = @()

function Nets($container) {
    $json = docker inspect --format '{{json .NetworkSettings.Networks}}' $container 2>$null
    if (-not $json) { throw "container '$container' not found" }
    ($json | ConvertFrom-Json).PSObject.Properties.Name
}

$scholar = "$Project-scholar"
$relay = "$Project-kagi-relay"

# 1. Scholar is up and healthy.  A scholar wired to an egress net kills itself
#    at boot (self-check), so a healthy scholar is itself evidence of
#    containment.
$state = docker inspect --format '{{.State.Health.Status}}' $scholar 2>$null
if ($state -ne 'healthy') { $fail += "scholar is not healthy (state='$state') — self-check may have refused to start" }

# 2. Topology: scholar off egress/statenet/buildnet, on kagiegress.
$snets = Nets $scholar
if ($snets -match '_egress$') { $fail += "scholar is ON an egress network: $($snets -match '_egress$')" }
if ($snets -match '_statenet$') { $fail += "scholar is ON statenet (route to builder/scribe)" }
if ($snets -match '_buildnet$') { $fail += "scholar is ON buildnet (route to builder/scribe/agent)" }
if (-not ($snets -match '_kagiegress$')) { $fail += "scholar is NOT on kagiegress (cannot reach the relay)" }

# 3. Only the relay holds egress.
$rnets = Nets $relay
$egressNet = $rnets -match '_egress$' | Select-Object -First 1
if (-not $egressNet) { $fail += "kagi-relay is NOT on an egress network" }
$kagiegress = $snets -match '_kagiegress$' | Select-Object -First 1

# 4. Active reachability from the scholar's actual net position (throwaway
#    alpine on the SAME internal net the scholar uses to reach the relay): the
#    internet is unreachable, the relay is reachable.
if ($kagiegress) {
    $toInternet = docker run --rm --network $kagiegress alpine:3.20 `
        sh -c "nc -z -w3 1.1.1.1 443 && echo OPEN || echo CLOSED" 2>$null
    if ($toInternet -match 'OPEN') { $fail += "from kagiegress, the public internet (1.1.1.1:443) is REACHABLE — the net is not isolated" }

    $toRelay = docker run --rm --network $kagiegress alpine:3.20 `
        sh -c "nc -z -w3 $relay 8443 && echo OPEN || echo CLOSED" 2>$null
    if ($toRelay -notmatch 'OPEN') { $fail += "from kagiegress, the kagi-relay ($relay`:8443) is UNREACHABLE" }
}

# 5. The relay's egress path actually reaches Kagi (and only via TLS:443).
if ($egressNet) {
    $toKagi = docker run --rm --network $egressNet alpine:3.20 `
        sh -c "nc -z -w5 kagi.com 443 && echo OPEN || echo CLOSED" 2>$null
    if ($toKagi -notmatch 'OPEN') { $fail += "from the egress net, kagi.com:443 is UNREACHABLE — the relay path is broken" }
}

if ($fail.Count -gt 0) {
    Write-Host "SCHOLAR CONTAINMENT DRIFT ($Project):" -ForegroundColor Red
    $fail | ForEach-Object { Write-Host "  - $_" -ForegroundColor Red }
    exit 1
}
Write-Host "scholar contained: no internet except via kagi-relay -> kagi.com; no builder/scribe route." -ForegroundColor Green
exit 0
