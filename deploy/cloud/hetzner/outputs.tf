output "name" {
  description = "The service name (NAME/something on tailnet devices)."
  value       = var.name
}

output "tag" {
  value = var.tailscale ? local.tag : null
}

output "server_type" {
  description = "The server type in use (picked from server_type_preference when the server was created)."
  value       = hcloud_server.server.server_type
}

output "arch" {
  value = local.arch
}

output "in_stock" {
  description = "Server types Hetzner reported as orderable in this location at plan time."
  value       = local.in_stock
}

locals {
  ipv4_address = var.ipv4 ? hcloud_server.server.ipv4_address : null
  ipv6_address = hcloud_server.server.ipv6_address
  ssh_host     = var.tailscale ? var.name : coalesce(local.ipv4_address, "[${local.ipv6_address}]")
  bootstrap = join(" ", compact([
    "sudo ./deploy/host/bootstrap.sh --admin-user admin --name ${var.name}",
    var.tailscale ? "--tag ${local.tag}" : "--no-tailscale --ssh-from any",
  ]))
}

output "ipv6_address" {
  description = "The server's public IPv6 address."
  value       = local.ipv6_address
}

output "ipv4_address" {
  value = local.ipv4_address
}

output "dns_records" {
  description = "With public_domain: create these at your DNS host."
  value = local.public ? join("\n", [
    "${var.public_domain}  A     ${local.ipv4_address}",
    "${var.public_domain}  AAAA  ${local.ipv6_address}",
  ]) : null
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
