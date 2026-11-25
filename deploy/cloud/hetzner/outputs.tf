output "name" {
  description = "Type NAME/something in a browser on any tailnet device."
  value       = var.name
}

output "tag" {
  value = local.tag
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

output "ipv6_address" {
  description = "The server's public IPv6 address. Nothing answers on it except Tailscale's WireGuard port."
  value       = hcloud_server.server.ipv6_address
}

output "ipv4_address" {
  value = var.ipv4 ? hcloud_server.server.ipv4_address : null
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
