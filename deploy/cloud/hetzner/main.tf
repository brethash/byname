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
    auth_key   = var.tailscale_auth_key
  })

  labels = { app = "ot", service = var.name }
}

resource "hcloud_ssh_key" "admin" {
  name       = "${var.name}-admin"
  public_key = trimspace(var.ssh_public_key)
  labels     = local.labels
}

# Inbound: only Tailscale's WireGuard port, for direct connections. WireGuard
# never answers a packet that isn't from a known peer. Outbound stays open
# (apt, Tailscale, S3).
resource "hcloud_firewall" "server" {
  name   = var.name
  labels = local.labels

  rule {
    direction   = "in"
    protocol    = "udp"
    port        = "41641"
    source_ips  = var.ipv4 ? ["0.0.0.0/0", "::/0"] : ["::/0"]
    description = "Tailscale direct connections (WireGuard)"
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
  }
}
