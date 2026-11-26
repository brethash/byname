/**
 * byname on Hetzner Cloud: one small server, in either of two setups (docs/cloud.md):
 *
 *  - your own domain (public_domain, ipv4 = true): TCP 80 and 443 are open to everyone, and
 *    OpenSSH (TCP 22) only to ssh_cidrs;
 *  - Tailscale (tailscale = true): IPv6-only is enough. Only Tailscale's WireGuard port is open,
 *    and you administer the server with Tailscale SSH. ssh_cidrs can add OpenSSH from your own
 *    addresses.
 *
 * The two can be combined. Without Tailscale, a domain and ssh_cidrs are required.
 *
 * The server type is picked at plan time: the first entry of `server_type_preference` (cheapest
 * first) that Hetzner can actually deliver in `location` right now. Hetzner's cheapest lines sell
 * out often (on 2026-11-24 its site listed CX23 and CAX11 as unavailable), so a fixed type would
 * fail as often as not.
 *
 * Prices on 2026-11-24, per month excluding VAT: CX23 €5.99 and CAX11 €6.49 (EU only), CPX12 €9.99
 * (hel1) or €11.99 (fsn1/nbg1), CPX11 €17.99 (ash/hil, the only US option), plus the IPv4 fee
 * with ipv4 = true. The same setups on AWS Lightsail (deploy/cloud/aws) are $3.50 and $5.00.
 *
 * State is local (terraform.tfstate here, gitignored) and stays on your machine: never copy
 * deploy/cloud to the server. It holds the server's user data, including the spent one-off
 * Tailscale auth key, and nothing else secret.
 */
terraform {
  required_version = ">= 1.6"

  required_providers {
    hcloud = {
      source  = "hetznercloud/hcloud"
      version = "~> 1.70"
    }
  }
}

provider "hcloud" {
  token = var.hcloud_token
}
