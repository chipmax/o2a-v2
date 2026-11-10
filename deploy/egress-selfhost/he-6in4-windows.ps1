#Requires -RunAsAdministrator
<#
.SYNOPSIS
    Hurricane Electric 6in4 tunnel for Windows 10/11 (behind a NAT firewall).

.DESCRIPTION
    Creates a protocol-41 (6in4) tunnel interface with the built-in Windows
    stack (netsh interface ipv6 add v6v4tunnel) - no third-party client.

    The Windows box keeps its LAN address; the edge firewall must forward
    IP protocol 41 to it (FortiGate: a WAN->host policy with service ALL and
    NAT disabled, or a DMZ host). See HE-TUNNEL-HOME.vi.md.

.EXAMPLE
    .\he-6in4-windows.ps1 -Action setup -LocalIPv4 192.168.110.88 `
        -Client6 2001:470:35:966::2 -TunnelId 1044203 `
        -RoutedPrefix 2001:470:36:965::/64

.EXAMPLE
    .\he-6in4-windows.ps1 -Action update -TunnelId 1044203 -Credential (Get-Credential)
#>
[CmdletBinding(DefaultParameterSetName = 'setup')]
param(
    [Parameter(Mandatory, ParameterSetName = 'setup')]
    [Parameter(Mandatory, ParameterSetName = 'status')]
    [Parameter(Mandatory, ParameterSetName = 'remove')]
    [ValidateSet('setup', 'status', 'remove', 'update')]
    [string]$Action = 'status',

    [string]$Server = '216.218.221.42',
    [string]$IfName = 'HE6in4',

    [Parameter(ParameterSetName = 'setup')]
    [string]$LocalIPv4,

    [Parameter(ParameterSetName = 'setup')]
    [string]$Client6,

    [Parameter(ParameterSetName = 'setup')]
    [string]$RoutedPrefix,

    [Parameter(Mandatory, ParameterSetName = 'update')]
    [string]$TunnelId,

    [Parameter(ParameterSetName = 'update')]
    [System.Management.Automation.PSCredential]$Credential,

    [Parameter(ParameterSetName = 'update')]
    [string]$MyIP
)

function Get-TunnelIfIndex {
    $line = netsh interface ipv6 show interfaces | Select-String -SimpleMatch $IfName
    if (-not $line) { return $null }
    return [int](($line -split '\s+')[1])
}

function Test-IPv6Address([string]$s) {
    $null = [System.Net.IPAddress]::Parse($s)
    return $true
}

switch ($Action) {
    'setup' {
        if (-not $LocalIPv4) { throw 'setup needs -LocalIPv4 (the LAN address of this PC).' }
        if (-not $Client6) { throw 'setup needs -Client6 (e.g. 2001:470:35:966::2).' }
        if (-not (Test-IPv6Address $Client6)) { throw "Client6 is not a valid IPv6 address: $Client6" }

        $existing = Get-TunnelIfIndex
        if ($existing) {
            Write-Host "removing previous tunnel interface '$IfName' (idx $existing)"
            netsh interface ipv6 delete interface interface=$IfName | Out-Null
        }

        Write-Host "creating 6in4 tunnel: local $LocalIPv4 -> $Server"
        netsh interface ipv6 add v6v4tunnel interface="$IfName" localaddress="$LocalIPv4" remoteaddress="$Server" | Out-Host
        netsh interface ipv6 add address interface="$IfName" address="$Client6" | Out-Host
        netsh interface ipv6 set interface interface="$IfName" metric=1 | Out-Host
        netsh interface ipv6 add route prefix=::/0 interface="$IfName" metric=1 | Out-Host

        if ($RoutedPrefix) {
            Write-Host "routed prefix $RoutedPrefix is assigned to HE; it is used on the tunnel peer,"
            Write-Host "not on this host. Add addresses from it only if you route it here."
        }

        Write-Host ''
        Write-Host 'Test with:  ping -6 2001:470:35:966::1   (HE server)'
        Write-Host '            curl.exe -6 https://api64.ipify.org'
    }

    'status' {
        netsh interface ipv6 show interfaces | Select-String -SimpleMatch $IfName
        netsh interface ipv6 show addresses | Select-String -Pattern '2001:470'
        netsh interface ipv6 show route | Select-String -Pattern '2001:470|::/0'
        $ip = (curl.exe -6 -s -m 8 https://api64.ipify.org)
        if ($ip) { Write-Host "public IPv6 egress: $ip" } else { Write-Host 'no IPv6 egress' }
    }

    'remove' {
        if (Get-TunnelIfIndex) {
            netsh interface ipv6 delete interface interface=$IfName | Out-Host
            Write-Host "removed $IfName"
        } else {
            Write-Host "$IfName not present"
        }
    }

    'update' {
        if (-not $MyIP) {
            $MyIP = (curl.exe -4 -s -m 8 https://api64.ipify.org)
        }
        if (-not $MyIP) { throw 'cannot determine the public IPv4; pass -MyIP explicitly.' }
        $uri = "https://ipv4.tunnelbroker.net/nic/update?hostname=$TunnelId&myip=$MyIP"
        $params = @{ Uri = $uri; Method = 'Get'; TimeoutSec = 20 }
        if ($Credential) { $params.Credential = $Credential }
        $resp = Invoke-WebRequest @params -UseBasicParsing
        Write-Host "endpoint $MyIP -> $($resp.StatusCode) $($resp.Content)"
    }
}
