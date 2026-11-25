output "name" {
  description = "Type NAME/something in a browser on any tailnet device."
  value       = var.name
}

output "tag" {
  value = local.tag
}

output "ipv6_address" {
  description = "The server's public IPv6 address. Nothing answers on it except Tailscale's WireGuard port."
  value       = try(aws_lightsail_instance.server.ipv6_addresses[0], null)
}

output "ipv4_address" {
  value = var.ipv4 ? aws_lightsail_instance.server.public_ip_address : null
}

output "next_steps" {
  description = "Run these from the repo root once the server shows up in your tailnet (a minute or two)."
  value       = <<-EOT
    tailscale ping ${var.name}
    scp -r deploy admin@${var.name}:
    ssh admin@${var.name} 'sudo ./deploy/host/bootstrap.sh --admin-user admin --name ${var.name} --tag ${local.tag}'
    make deploy TARGET=admin@${var.name}
    ssh admin@${var.name} sudo ot-verify
  EOT
}

output "estimated_monthly_cost_usd" {
  description = "Lightsail list price for the bundle (2026-11-24). S3 backups add well under a cent."
  value       = lookup(local.bundle_prices, local.bundle_id, "see: aws lightsail get-bundles")
}
