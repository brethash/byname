variable "name" {
  description = <<-EOT
    The service name, e.g. "me" for me/wiki. It becomes the server's hostname and the Lightsail
    instance name, and with Tailscale its tailnet (MagicDNS) name: nothing else on the tailnet may
    already use it, or Tailscale renames this server to NAME-1.
  EOT
  type        = string
  default     = "me"

  validation {
    condition     = can(regex("^[a-z]([a-z0-9-]{0,30}[a-z0-9])?$", var.name)) && var.name != "localhost"
    error_message = "name must be 1-32 lower-case letters, digits or dashes, starting with a letter."
  }
}

variable "tailscale" {
  description = <<-EOT
    Join your tailnet on first boot (needs tailscale_auth_key). Tailnet devices then use NAME/wiki
    with no sign-in, and you administer the server with Tailscale SSH. Without it, you reach the
    server with OpenSSH from ssh_cidrs, and people use public_domain.
  EOT
  type        = bool
  default     = false
}

variable "public_domain" {
  description = <<-EOT
    Your own domain for the service, e.g. "hg.example.com". byname serves it directly over HTTPS with
    a Let's Encrypt certificate, and everyone outside the tailnet signs in with Google or Slack
    (docs/public-access.md). Opens TCP 80 and 443 to the internet and needs ipv4 = true. Point
    the domain's A and AAAA records at the addresses in the outputs.
  EOT
  type        = string
  default     = ""

  validation {
    condition     = var.public_domain == "" || can(regex("^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\\.)+[a-z]{2,63}$", var.public_domain))
    error_message = "public_domain must be a lower-case DNS name like hg.example.com (no scheme, no path)."
  }
}

variable "ssh_cidrs" {
  description = <<-EOT
    Addresses allowed to reach OpenSSH from the internet, e.g. ["203.0.113.7/32"] for your home
    IPv4 (curl -s https://checkip.amazonaws.com). Required without Tailscale; leave empty with
    Tailscale, which reaches SSH over the tailnet instead. Never use 0.0.0.0/0.
  EOT
  type        = list(string)
  default     = []

  validation {
    # Canonical CIDR blocks (no host bits), at most a /16 for IPv4 and a /48
    # for IPv6: this is the only address filter for SSH with --ssh-from any.
    condition = alltrue([for c in var.ssh_cidrs : try(
      cidrhost(c, 0) == split("/", c)[0] && tonumber(split("/", c)[1]) >= (strcontains(c, ":") ? 48 : 16),
      false
    )])
    error_message = "ssh_cidrs must be CIDR blocks like 203.0.113.7/32 (no host bits), no wider than /16 for IPv4 or /48 for IPv6."
  }
}

variable "tag" {
  description = "Tailscale tag for the server. Defaults to tag:NAME. Your tailnet policy must define it (deploy/tailscale/policy.hujson)."
  type        = string
  default     = null

  validation {
    condition     = var.tag == null || can(regex("^tag:[a-z0-9]([a-z0-9-]*[a-z0-9])?$", var.tag))
    error_message = "tag must look like tag:me."
  }
}

variable "tailscale_auth_key" {
  description = <<-EOT
    With tailscale = true: a one-off Tailscale auth key (tskey-auth-…) that lets the server join
    your tailnet on first boot. Create it at login.tailscale.com/admin/settings/keys: not
    reusable, not ephemeral, pre-approved, tagged with the server's tag. Pass it as
    TF_VAR_tailscale_auth_key.

    It is only read when the server is created (later changes to user data are ignored), so on
    later applies any well-formed value will do, e.g. tskey-auth-unused.
  EOT
  type        = string
  sensitive   = true
  default     = ""

  validation {
    condition     = var.tailscale_auth_key == "" || can(regex("^tskey-auth-[A-Za-z0-9-]+$", var.tailscale_auth_key))
    error_message = "tailscale_auth_key must be a one-off auth key (tskey-auth-…). OAuth client secrets (tskey-client-…) are reusable and never expire, so they must not go into user data."
  }
}

variable "ssh_public_key" {
  description = <<-EOT
    Your OpenSSH public key (the contents of ~/.ssh/id_ed25519.pub), installed for the "admin"
    user. Without Tailscale it is how you log in (from ssh_cidrs); with Tailscale it is the
    fallback over the tailnet.
  EOT
  type        = string

  validation {
    condition     = can(regex("^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(256|384|521)|sk-ssh-ed25519@openssh\\.com|sk-ecdsa-sha2-nistp256@openssh\\.com) [A-Za-z0-9+/]+=*( [^'\\r\\n]*)?$", trimspace(var.ssh_public_key)))
    error_message = "ssh_public_key must be one OpenSSH public key line (not a path, not a private key)."
  }
}

variable "region" {
  description = "AWS region. Pick the one nearest the household: every redirect makes a round trip to it."
  type        = string
  default     = "us-west-2"
}

variable "availability_zone" {
  description = "Lightsail availability zone. Defaults to the region's first zone (REGIONa)."
  type        = string
  default     = null
}

variable "ipv4" {
  description = <<-EOT
    Give the server a public IPv4 address too (dual-stack). It adds $1.50/month (nano_3_0 is
    $5.00 against $3.50 for nano_ipv6_3_0). Required for public_domain (most of the internet
    still reaches websites over IPv4) and for Sign in with Slack (slack.com has no IPv6 address).
    A tailnet-only server doesn't need it: Tailscale clients on IPv4-only networks reach an
    IPv6-only server through Tailscale's relays.

    Decide at creation: switching later means a different bundle, which replaces the server.
  EOT
  type        = bool
  default     = false
}

variable "bundle_id" {
  description = <<-EOT
    Lightsail bundle. Defaults to the cheapest: nano_ipv6_3_0 ($3.50/month), or nano_3_0
    ($5.00) when ipv4 = true. `aws lightsail get-bundles` lists the others. Changing it replaces
    the server (restore from backup afterwards).
  EOT
  type        = string
  default     = null
}

variable "blueprint_id" {
  description = "Lightsail OS image. Debian 13 is listed by AWS as supported on IPv6-only plans; only Debian is supported by bootstrap.sh."
  type        = string
  default     = "debian_13"

  validation {
    condition     = startswith(var.blueprint_id, "debian_")
    error_message = "bootstrap.sh supports Debian only."
  }
}
