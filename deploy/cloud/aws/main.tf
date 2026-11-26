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

  # Lightsail keeps separate IPv4 and IPv6 firewalls, and a port rule that
  # leaves a family out opens the port to that whole family (0.0.0.0/0 or
  # ::/0). The provider drops an empty list, so "nobody" can't be written as
  # []. A family that ssh_cidrs leaves empty gets a documentation address
  # instead (RFC 5737, RFC 3849): it is never routed on the internet, so no
  # connection can come from it.
  nobody_ipv4 = ["192.0.2.0/32"]
  nobody_ipv6 = ["2001:db8::/128"]

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
      # An IPv6-only plan has no IPv4 address and no IPv4 firewall to set.
      condition     = var.ipv4 || length(local.ssh_ipv4) == 0
      error_message = "ipv4 = false gives the server no IPv4 address, so ssh_cidrs can only list IPv6 blocks (or set ipv4 = true)."
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

  lifecycle {
    # A replacement server keeps the same name, so nothing here would change
    # and the new server would be left without its static IP.
    replace_triggered_by = [aws_lightsail_instance.server.arn]
  }
}

# Replaces Lightsail's default firewall (which opens 22 and 80 to the world)
# with only what this server needs: Tailscale's WireGuard port (which never
# answers a packet that isn't from a known peer), OpenSSH from ssh_cidrs
# (always without Tailscale, optionally with it), and HTTP/HTTPS for a public
# domain.
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

  # Both families are always spelled out (see nobody_ipv4 above). On an
  # IPv6-only plan there is no IPv4 firewall, so only IPv6 is sent.
  dynamic "port_info" {
    for_each = length(var.ssh_cidrs) > 0 ? [1] : []
    content {
      protocol   = "tcp"
      from_port  = 22
      to_port    = 22
      cidrs      = var.ipv4 ? coalescelist(local.ssh_ipv4, local.nobody_ipv4) : null
      ipv6_cidrs = coalescelist(local.ssh_ipv6, local.nobody_ipv6)
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
    # A replacement server keeps the same name, so nothing here would change
    # and the new server would keep AWS's default firewall (SSH and HTTP open
    # to everyone).
    replace_triggered_by = [aws_lightsail_instance.server.arn]

    # The backstop, checked against what AWS actually stored: SSH must never
    # be open to the whole internet. IPv4 is checked only on a dual-stack
    # server; an IPv6-only plan has no IPv4 firewall, and AWS may report its
    # default for one.
    postcondition {
      condition = alltrue([for p in self.port_info : !(contains(["tcp", "all", "-1"], p.protocol) && p.from_port <= 22 && p.to_port >= 22) || (
        !(var.ipv4 && try(contains(p.cidrs, "0.0.0.0/0"), false)) && !try(contains(p.ipv6_cidrs, "::/0"), false)
      )])
      error_message = "AWS's firewall now lets SSH (port 22) in from the whole internet. Check: aws lightsail get-instance-port-states --instance-name ${var.name}"
    }
  }
}

# Lightsail has no delete protection, so this stands in for Hetzner's: with
# protect = true (the default), an apply that would destroy or replace the
# server (a changed name, bundle_id, ipv4 or availability_zone, -replace, or
# tofu destroy) stops here before anything is touched. Every other resource
# here is a dependency of this one, so OpenTofu always destroys this first.
resource "terraform_data" "protect" {
  input            = var.protect
  triggers_replace = aws_lightsail_instance.server.arn
  depends_on       = [aws_lightsail_instance_public_ports.server, aws_lightsail_static_ip_attachment.server]

  provisioner "local-exec" {
    when       = destroy
    on_failure = fail
    command    = self.input ? "echo 'protect = true: this would destroy the server and its database. To do it on purpose, run tofu apply -var protect=false with no other change, then your command again with -var protect=false (docs/cloud.md, Replacing or tearing down the server).' >&2; exit 1" : "true"
  }
}
