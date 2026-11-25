/**
 * byname on AWS: one Lightsail nano instance, IPv6-only, reachable only over Tailscale.
 *
 * Why Lightsail and not EC2: the IPv6-only nano bundle is $3.50/month flat (512 MB, 2 vCPU burst,
 * 20 GB SSD, 1 TB transfer). The closest EC2 setup, a t4g.nano with an 8 GB gp3 disk, is $3.71
 * IPv6-only, or $7.36 with the $3.65/month public IPv4 address. And AWS documents Debian 13 as
 * supported on Lightsail's IPv6-only plans, while IPv6-only Debian on plain EC2 has had cloud-init
 * metadata problems. What EC2 would add is an instance role for backups; here the backup unit
 * uses the same put-only access key as the Pi.
 *
 * Nothing listens publicly except Tailscale's WireGuard port, which ignores anything that is not
 * a known peer. Shell access is Tailscale SSH; there is no public port 22.
 *
 * State is local (terraform.tfstate here, gitignored). It holds the one-off Tailscale auth key,
 * which is spent by the time apply finishes, and nothing else secret.
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
