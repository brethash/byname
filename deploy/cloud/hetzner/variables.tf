variable "hcloud_token" {
  description = "Hetzner Cloud API token (Read & Write) from the project's Security → API tokens page. Pass it as TF_VAR_hcloud_token."
  type        = string
  sensitive   = true
}

variable "name" {
  description = <<-EOT
    The service name people type, e.g. "ot" for ot/wiki. It becomes the server's hostname, its
    tailnet (MagicDNS) name and the Hetzner server name. Nothing else on the tailnet may already
    use it, or Tailscale renames this server to NAME-1.
  EOT
  type        = string
  default     = "ot"

  validation {
    condition     = can(regex("^[a-z]([a-z0-9-]{0,30}[a-z0-9])?$", var.name)) && var.name != "localhost"
    error_message = "name must be 1-32 lower-case letters, digits or dashes, starting with a letter."
  }
}

variable "tag" {
  description = "Tailscale tag for the server. Defaults to tag:NAME. Your tailnet policy must define it (deploy/tailscale/policy.hujson)."
  type        = string
  default     = null

  validation {
    condition     = var.tag == null || can(regex("^tag:[a-z0-9]([a-z0-9-]*[a-z0-9])?$", var.tag))
    error_message = "tag must look like tag:ot."
  }
}

variable "tailscale_auth_key" {
  description = <<-EOT
    One-off Tailscale auth key (tskey-auth-…) that lets the server join your tailnet on first boot.
    Create it at login.tailscale.com/admin/settings/keys: not reusable, not ephemeral,
    pre-approved, tagged with the server's tag. Pass it as TF_VAR_tailscale_auth_key.

    It is only read when the server is created (later changes to user data are ignored), so on
    later applies any well-formed value will do, e.g. tskey-auth-unused.
  EOT
  type        = string
  sensitive   = true

  validation {
    condition     = can(regex("^tskey-(auth|client)-[A-Za-z0-9-]+(\\?[A-Za-z0-9=&_-]+)?$", var.tailscale_auth_key))
    error_message = "tailscale_auth_key must be a Tailscale auth key (tskey-auth-…)."
  }
}

variable "ssh_public_key" {
  description = <<-EOT
    Your OpenSSH public key (the contents of ~/.ssh/id_ed25519.pub), installed for the "admin"
    user that first boot creates. Registering it with Hetzner also stops Hetzner from emailing a
    root password. Day to day you log in with Tailscale SSH; this key is the fallback over the
    tailnet.
  EOT
  type        = string

  validation {
    condition     = can(regex("^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(256|384|521)|sk-ssh-ed25519@openssh\\.com|sk-ecdsa-sha2-nistp256@openssh\\.com) [A-Za-z0-9+/]+=*( [^'\\r\\n]*)?$", trimspace(var.ssh_public_key)))
    error_message = "ssh_public_key must be one OpenSSH public key line (not a path, not a private key)."
  }
}

variable "location" {
  description = <<-EOT
    Hetzner location. hel1 (Helsinki) is the cheapest: CPX12 is €9.99 there against €11.99 in
    fsn1/nbg1, and CX23/CAX11 (€5.99/€6.49, when in stock) are sold in nbg1 and hel1. The US
    sites ash (Ashburn) and hil (Hillsboro) only offer CPX11 at €17.99. Every redirect makes a round
    trip to the server, so from North America an EU location adds roughly 100-150 ms per link.
  EOT
  type        = string
  default     = "hel1"

  validation {
    condition     = contains(["fsn1", "nbg1", "hel1", "ash", "hil", "sin"], var.location)
    error_message = "location must be one of fsn1, nbg1, hel1, ash, hil, sin."
  }
}

variable "server_type_preference" {
  description = <<-EOT
    Server types to try, cheapest first. The first one Hetzner reports as available (and not
    deprecated) in `location` is used. ot needs about 64 MB of RAM, so the smallest of any line
    is plenty. Both architectures work: deploy.sh ships the matching binary.
  EOT
  type        = list(string)
  default     = ["cx23", "cax11", "cpx12", "cpx11", "cpx22", "cpx21"]

  validation {
    condition     = length(var.server_type_preference) > 0
    error_message = "List at least one server type."
  }
}

variable "server_type" {
  description = "Use this server type instead of picking from server_type_preference."
  type        = string
  default     = null
}

variable "ipv4" {
  description = <<-EOT
    Give the server a public IPv4 address too. Off by default: nothing needs it, and Hetzner
    bills primary IPv4 addresses separately. Tailscale clients on IPv4-only networks still reach
    an IPv6-only server through Tailscale's relays, with a few milliseconds more latency.
  EOT
  type        = bool
  default     = false
}

variable "image" {
  description = "OS image. bootstrap.sh supports Debian only."
  type        = string
  default     = "debian-13"

  validation {
    condition     = startswith(var.image, "debian-")
    error_message = "bootstrap.sh supports Debian only."
  }
}

variable "hetzner_backups" {
  description = <<-EOT
    Hetzner's daily server backups, billed at 20% of the server price. Off by default: ot's own
    snapshots go to S3 (docs/operations.md → Backups) for a fraction of a cent, and restore
    anywhere, not only on Hetzner.
  EOT
  type        = bool
  default     = false
}

variable "protect" {
  description = "Hetzner delete and rebuild protection. The database lives on this server's disk; turn this off only to tear it down."
  type        = bool
  default     = true
}
