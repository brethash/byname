# Offline checks of the Hetzner firewall and next_steps, against a mock
# provider: nothing here contacts Hetzner. Run from deploy/cloud/hetzner:
#   tofu init -backend=false && tofu test

mock_provider "hcloud" {
  mock_resource "hcloud_firewall" {
    defaults = { id = "1" }
  }
  mock_resource "hcloud_ssh_key" {
    defaults = { id = "2" }
  }
  mock_resource "hcloud_server" {
    defaults = {
      ipv4_address = "198.51.100.10"
      ipv6_address = "2001:db8:5::1"
    }
  }
}

variables {
  hcloud_token   = "test"
  server_type    = "cx23" # the mock lists no server types
  name           = "me"
  ssh_public_key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleKeyExampleKeyExampleKeyExampleKey00 you@laptop"
  # tofu test destroys what it applied; Hetzner's protection is the provider's.
  protect = false
}

run "domain" {
  command = apply
  variables {
    public_domain = "me.example.com"
    ipv4          = true
    ssh_cidrs     = ["203.0.113.7/32"]
  }
  assert {
    condition     = one([for r in hcloud_firewall.server.rule : r.source_ips if r.port == "22"]) == toset(["203.0.113.7/32"])
    error_message = "Hetzner's SSH rule must list exactly ssh_cidrs."
  }
  assert {
    condition = (
      strcontains(output.next_steps, "git archive HEAD deploy/host deploy/systemd | ssh admin@198.51.100.10 tar -x") &&
      strcontains(output.next_steps, "--no-tailscale --ssh-from any'") && !strcontains(output.next_steps, "scp")
    )
    error_message = "next_steps must ship only deploy/host and deploy/systemd, and bootstrap with --ssh-from any."
  }
}

run "domain_ipv6_ssh" {
  command = apply
  variables {
    public_domain = "me.example.com"
    ipv4          = true
    ssh_cidrs     = ["2001:db8:1234::/48"]
  }
  assert {
    condition     = strcontains(output.next_steps, "| ssh admin@2001:db8:5::1 tar -x") && !strcontains(output.next_steps, "[")
    error_message = "With only IPv6 in ssh_cidrs, next_steps must use the bare IPv6 address."
  }
}

run "tailscale" {
  command = apply
  variables {
    tailscale          = true
    tailscale_auth_key = "tskey-auth-test"
  }
  assert {
    condition     = strcontains(output.next_steps, "| ssh admin@me tar -x") && !strcontains(output.next_steps, "--ssh-from")
    error_message = "Tailscale-only next_steps ship over the tailnet name, without --ssh-from."
  }
}
