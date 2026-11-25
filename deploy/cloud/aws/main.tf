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
    auth_key   = var.tailscale ? var.tailscale_auth_key : ""
  })

  public   = var.public_domain != ""
  ssh_ipv4 = [for c in var.ssh_cidrs : c if !strcontains(c, ":")]
  ssh_ipv6 = [for c in var.ssh_cidrs : c if strcontains(c, ":")]

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
    precondition {
      condition     = !var.tailscale || var.tailscale_auth_key != ""
      error_message = "tailscale = true needs tailscale_auth_key (TF_VAR_tailscale_auth_key=tskey-auth-…)."
    }
    precondition {
      condition     = var.tailscale || length(var.ssh_cidrs) > 0
      error_message = "Without Tailscale, set ssh_cidrs to your own address (e.g. [\"203.0.113.7/32\"]) so you can reach the server."
    }
    precondition {
      condition     = var.tailscale || local.public
      error_message = "Without Tailscale, set public_domain: otherwise nobody can use the service."
    }
    precondition {
      condition     = !local.public || var.ipv4
      error_message = "public_domain needs ipv4 = true: most of the internet still reaches websites over IPv4, and Sign in with Slack needs it."
    }
    precondition {
      # Lightsail's IPv4 and IPv6 firewalls are separate, and AWS doesn't
      # document what an SSH rule without IPv4 addresses allows over IPv4.
      condition     = !var.ipv4 || length(var.ssh_cidrs) == 0 || length(local.ssh_ipv4) > 0
      error_message = "With ipv4 = true, ssh_cidrs needs at least one IPv4 block (your home IPv4, e.g. 203.0.113.7/32)."
    }
  }
}

# A fixed IPv4 address for the domain's A record. Free while attached.
resource "aws_lightsail_static_ip" "server" {
  count = local.public ? 1 : 0
  name  = "${var.name}-ip"
}

resource "aws_lightsail_static_ip_attachment" "server" {
  count          = local.public ? 1 : 0
  static_ip_name = aws_lightsail_static_ip.server[0].name
  instance_name  = aws_lightsail_instance.server.name
}

# Replaces Lightsail's default firewall (which opens 22 and 80 to the world)
# with only what this server needs: Tailscale's WireGuard port (which never
# answers a packet that isn't from a known peer), OpenSSH from your addresses
# when there's no Tailscale, and HTTP/HTTPS for a public domain.
resource "aws_lightsail_instance_public_ports" "server" {
  instance_name = aws_lightsail_instance.server.name

  dynamic "port_info" {
    for_each = var.tailscale ? [1] : []
    content {
      protocol   = "udp"
      from_port  = 41641
      to_port    = 41641
      cidrs      = var.ipv4 ? ["0.0.0.0/0"] : null
      ipv6_cidrs = ["::/0"]
    }
  }

  dynamic "port_info" {
    for_each = length(var.ssh_cidrs) > 0 ? [1] : []
    content {
      protocol   = "tcp"
      from_port  = 22
      to_port    = 22
      cidrs      = length(local.ssh_ipv4) > 0 ? local.ssh_ipv4 : null
      ipv6_cidrs = length(local.ssh_ipv6) > 0 ? local.ssh_ipv6 : null
    }
  }

  dynamic "port_info" {
    for_each = local.public ? [80, 443] : []
    content {
      protocol   = "tcp"
      from_port  = port_info.value
      to_port    = port_info.value
      cidrs      = ["0.0.0.0/0"]
      ipv6_cidrs = ["::/0"]
    }
  }

  lifecycle {
    # Checked after every apply, against what AWS actually stored: SSH must
    # never be open to the whole internet, on IPv4 or IPv6.
    postcondition {
      condition = alltrue([for p in self.port_info : !(p.protocol == "tcp" && p.from_port <= 22 && p.to_port >= 22) || (
        !try(contains(p.cidrs, "0.0.0.0/0"), false) && !try(contains(p.ipv6_cidrs, "::/0"), false)
      )])
      error_message = "AWS's firewall now lets SSH (port 22) in from the whole internet. Check: aws lightsail get-instance-port-states --instance-name ${var.name}"
    }
  }
}
