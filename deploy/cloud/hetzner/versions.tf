/**
 * byname on Hetzner Cloud: one small server, IPv6-only, reachable only over Tailscale.
 *
 * The server type is picked at plan time: the first entry of `server_type_preference` (cheapest
 * first) that Hetzner can actually deliver in `location` right now. Hetzner's cheapest lines sell
 * out often (on 2026-11-24 its site listed CX23 and CAX11 as unavailable), so a fixed type would
 * fail as often as not.
 *
 * Prices on 2026-11-24, per month excluding VAT: CX23 €5.99 and CAX11 €6.49 (EU only), CPX12 €9.99
 * (hel1) or €11.99 (fsn1/nbg1), CPX11 €17.99 (ash/hil, the only US option). The same household
 * setup on AWS Lightsail (deploy/cloud/aws) is $3.50.
 *
 * Nothing listens publicly except Tailscale's WireGuard port. Shell access is Tailscale SSH.
 * State is local and gitignored; it holds the spent one-off auth key and nothing else secret.
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
