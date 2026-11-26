/**
 * byname on AWS: one Lightsail nano instance, in either of two setups (docs/cloud.md):
 *
 *  - your own domain (public_domain, ipv4 = true): dual-stack, $5.00/month. TCP 80 and 443 are
 *    open to everyone, and OpenSSH (TCP 22) only to ssh_cidrs, on both IPv4 and IPv6;
 *  - Tailscale (tailscale = true): IPv6-only is enough, $3.50/month. Only Tailscale's WireGuard
 *    port is open, and it ignores anything that is not a known peer; you administer the server
 *    with Tailscale SSH. ssh_cidrs can add OpenSSH from your own addresses.
 *
 * The two can be combined. Without Tailscale, a domain and ssh_cidrs are required.
 *
 * Why Lightsail and not EC2: the IPv6-only nano bundle is $3.50/month flat (512 MB, 2 vCPU burst,
 * 20 GB SSD, 1 TB transfer). The closest EC2 setup, a t4g.nano with an 8 GB gp3 disk, is $3.71
 * IPv6-only, or $7.36 with the $3.65/month public IPv4 address. And AWS documents Debian 13 as
 * supported on Lightsail's IPv6-only plans, while IPv6-only Debian on plain EC2 has had cloud-init
 * metadata problems. What EC2 would add is an instance role for backups; here the backup unit
 * uses the same put-only access key as the Pi.
 *
 * State is local (terraform.tfstate here, gitignored) and stays on your machine: never copy
 * deploy/cloud to the server. It holds the server's user data, including the one-off Tailscale
 * auth key (spent by the time apply finishes), and nothing else secret.
 */
terraform {
  required_version = ">= 1.6"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
  }
}

provider "aws" {
  region = var.region

  default_tags {
    tags = {
      app     = "byname"
      service = var.name
    }
  }
}
