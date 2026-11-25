locals {
  tag       = coalesce(var.tag, "tag:${var.name}")
  bundle_id = coalesce(var.bundle_id, var.ipv4 ? "nano_3_0" : "nano_ipv6_3_0")
  zone      = coalesce(var.availability_zone, "${var.region}a")

  # Lightsail's Debian images log in as "admin"; first-boot.sh adds your key to it.
  admin_user = "admin"

  user_data = templatefile("${path.module}/../user-data.sh.tftpl", {
    script     = file("${path.module}/../first-boot.sh")
    name       = var.name
    tag        = local.tag
    admin_user = local.admin_user
    ssh_key    = trimspace(var.ssh_public_key)
    auth_key   = var.tailscale_auth_key
  })

  # Monthly list prices (USD) on 2026-11-24, from `aws lightsail get-bundles`.
  bundle_prices = {
    nano_ipv6_3_0  = 3.50
    nano_3_0       = 5.00
    micro_ipv6_3_0 = 5.00
    micro_3_0      = 7.00
  }
}

# Lightsail requires a key pair on every instance; this one is yours, so no
# AWS-held private key exists for the server.
resource "aws_lightsail_key_pair" "admin" {
  name       = "${var.name}-admin"
  public_key = trimspace(var.ssh_public_key)
}

resource "aws_lightsail_instance" "server" {
  name              = var.name
  availability_zone = local.zone
  blueprint_id      = var.blueprint_id
  bundle_id         = local.bundle_id
  ip_address_type   = var.ipv4 ? "dualstack" : "ipv6"
  key_pair_name     = aws_lightsail_key_pair.admin.name
  user_data         = local.user_data

  lifecycle {
    # The database lives on this server's disk, so nothing routine may
    # replace it. User data only matters at first boot (and carries a spent
    # auth key afterwards); a newer blueprint is picked up by apt, not by
    # rebuilding.
    ignore_changes = [user_data, blueprint_id, key_pair_name]

    precondition {
      condition     = strcontains(local.bundle_id, "_ipv6_") == !var.ipv4
      error_message = "bundle_id ${local.bundle_id} does not match ipv4 = ${var.ipv4}: IPv6-only servers need an *_ipv6_* bundle, dual-stack servers one without."
    }
  }
}

# Replaces Lightsail's default firewall (which opens 22 and 80 to the world)
# with exactly one rule: Tailscale's WireGuard port, for direct connections.
# WireGuard never answers a packet that isn't from a known peer.
resource "aws_lightsail_instance_public_ports" "server" {
  instance_name = aws_lightsail_instance.server.name

  port_info {
    protocol   = "udp"
    from_port  = 41641
    to_port    = 41641
    cidrs      = var.ipv4 ? ["0.0.0.0/0"] : null
    ipv6_cidrs = ["::/0"]
  }
}
