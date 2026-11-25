locals {
  ipv4_address = local.public ? aws_lightsail_static_ip.server[0].ip_address : (var.ipv4 ? aws_lightsail_instance.server.public_ip_address : null)
  ipv6_address = try(aws_lightsail_instance.server.ipv6_addresses[0], null)
  # How you reach the server for setup: its tailnet name, or its address.
  ssh_host = var.tailscale ? var.name : coalesce(local.ipv4_address, "[${local.ipv6_address}]")
  bootstrap = join(" ", compact([
    "sudo ./deploy/host/bootstrap.sh --admin-user admin --name ${var.name}",
    var.tailscale ? "--tag ${local.tag}" : "--no-tailscale --ssh-from any",
  ]))
}

output "name" {
  description = "The service name (NAME/something on tailnet devices)."
  value       = var.name
}

output "tag" {
  value = var.tailscale ? local.tag : null
}

output "ipv6_address" {
  description = "The server's public IPv6 address."
  value       = local.ipv6_address
}

output "ipv4_address" {
  description = "The server's public IPv4 address (a static IP with public_domain)."
  value       = local.ipv4_address
}

output "dns_records" {
  description = "With public_domain: create these at your DNS host."
  value = local.public ? join("\n", compact([
    "${var.public_domain}  A     ${local.ipv4_address}",
    local.ipv6_address == null ? "" : "${var.public_domain}  AAAA  ${local.ipv6_address}",
  ])) : null
}

output "next_steps" {
  description = "Run these from the repo root once the server is up (a minute or two)."
  value = var.tailscale ? join("\n", compact([
    "tailscale ping ${var.name}",
    "scp -r deploy admin@${var.name}:",
    "ssh admin@${var.name} '${local.bootstrap}'",
    "make deploy TARGET=admin@${var.name}",
    "ssh admin@${var.name} sudo byname-verify",
    local.public ? "# then sign-in and --public-domain ${var.public_domain}: docs/public-access.md" : "",
    ])) : join("\n", [
    "# 1. DNS: create the records in dns_records, then wait for them to resolve.",
    "scp -r deploy admin@${local.ssh_host}:",
    "ssh admin@${local.ssh_host} '${local.bootstrap}'",
    "# 2. Sign-in settings and secret on the server: docs/public-access.md",
    "ssh admin@${local.ssh_host} '${local.bootstrap} --public-domain ${var.public_domain}'",
    "make deploy TARGET=admin@${local.ssh_host}",
    "ssh admin@${local.ssh_host} sudo byname-verify",
  ])
}

output "estimated_monthly_cost_usd" {
  description = "Lightsail list price for the bundle (2026-11-24). A static IP is free while attached; S3 backups add well under a cent."
  value       = lookup(local.bundle_prices, local.bundle_id, "see: aws lightsail get-bundles")
}
