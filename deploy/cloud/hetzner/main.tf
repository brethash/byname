data "hcloud_server_types" "all" {}

locals {
  tag        = coalesce(var.tag, "tag:${var.name}")
  admin_user = "admin"

  # Server types Hetzner can deliver in var.location right now.
  in_stock = [
    for t in data.hcloud_server_types.all.server_types : t.name
    if anytrue([for l in t.locations : l.name == var.location && l.available && !l.is_deprecated])
  ]
  cheapest = [for n in var.server_type_preference : n if contains(local.in_stock, n)]

  # If nothing on the list is in stock, ask for the first anyway: Hetzner's
  # "unavailable" error says more than a failed precondition would.
  server_type = coalesce(var.server_type, try(local.cheapest[0], null), var.server_type_preference[0])
  arch        = try(one([for t in data.hcloud_server_types.all.server_types : t.architecture if t.name == local.server_type]), null)

  user_data = templatefile("${path.module}/../user-data.sh.tftpl", {
    script     = file("${path.module}/../first-boot.sh")
    name       = var.name
    tag        = local.tag
    admin_user = local.admin_user
    ssh_key    = trimspace(var.ssh_public_key)
    auth_key   = var.tailscale ? var.tailscale_auth_key : ""
  })

  public = var.public_domain != ""

  labels = { app = "nym", service = var.name }
}

resource "hcloud_ssh_key" "admin" {
  name       = "${var.name}-admin"
  public_key = trimspace(var.ssh_public_key)
  labels     = local.labels
}

# Inbound: only what this server needs. Tailscale's WireGuard port (which
# never answers a packet that isn't from a known peer), OpenSSH from your
# addresses when there's no Tailscale, and HTTP/HTTPS for a public domain.
# Outbound stays open (apt, Tailscale, S3, Let's Encrypt, sign-in).
resource "hcloud_firewall" "server" {
  name   = var.name
  labels = local.labels

  dynamic "rule" {
    for_each = var.tailscale ? [1] : []
    content {
      direction   = "in"
      protocol    = "udp"
      port        = "41641"
      source_ips  = var.ipv4 ? ["0.0.0.0/0", "::/0"] : ["::/0"]
      description = "Tailscale direct connections (WireGuard)"
    }
  }

  dynamic "rule" {
    for_each = length(var.ssh_cidrs) > 0 ? [1] : []
    content {
      direction   = "in"
      protocol    = "tcp"
      port        = "22"
      source_ips  = var.ssh_cidrs
      description = "OpenSSH from your addresses"
    }
  }

  dynamic "rule" {
    for_each = local.public ? ["80", "443"] : []
    content {
      direction   = "in"
      protocol    = "tcp"
      port        = rule.value
      source_ips  = ["0.0.0.0/0", "::/0"]
      description = "byname public domain"
    }
  }
}

resource "hcloud_server" "server" {
  name               = var.name
  server_type        = local.server_type
  location           = var.location
  image              = var.image
  ssh_keys           = [hcloud_ssh_key.admin.id]
  firewall_ids       = [hcloud_firewall.server.id]
  backups            = var.hetzner_backups
  delete_protection  = var.protect
  rebuild_protection = var.protect
  keep_disk          = true
  user_data          = local.user_data
  labels             = local.labels

  public_net {
    ipv4_enabled = var.ipv4
    ipv6_enabled = true
  }

  lifecycle {
    # The database lives on this server's disk, so nothing routine may
    # replace or resize it: user data only matters at first boot, images are
    # updated by apt, and which type was cheapest-in-stock changes daily.
    # Resize deliberately with `hcloud server change-type --keep-disk`.
    ignore_changes = [user_data, image, ssh_keys, server_type]

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
  }
}
