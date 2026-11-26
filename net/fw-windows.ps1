# Abre no firewall do Windows a entrada TCP das portas do Nginx do testbed (rede espelhada do WSL2).
# Idempotente: remove a regra anterior e recria. Rodar como administrador.
$ErrorActionPreference = 'Stop'

$RuleName = 'Testbed-Unidade3-Nginx'
$Ports = '8081-8083'

$principal = [Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw "fw-windows.ps1 precisa de administrador"
}

Remove-NetFirewallRule -DisplayName $RuleName -ErrorAction SilentlyContinue
New-NetFirewallRule -DisplayName $RuleName -Direction Inbound -Protocol TCP -LocalPort $Ports -Profile Any -Action Allow | Out-Null
Write-Output "OK regra de firewall $RuleName (TCP $Ports, entrada, qualquer perfil)"
