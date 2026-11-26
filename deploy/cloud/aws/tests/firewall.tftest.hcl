# Offline checks of the Lightsail firewall rules and preconditions, against a
# mock AWS provider: nothing here contacts AWS. Run from deploy/cloud/aws:
#   tofu init -backend=false && tofu test

mock_provider "aws" {
  mock_resource "aws_lightsail_instance" {
    defaults = {
      arn               = "arn:aws:lightsail:us-west-2:123456789012:Instance/00000000-0000-0000-0000-000000000000"
      ipv6_addresses    = ["2001:db8:5::10"]
      public_ip_address = "198.51.100.10"
    }
  }
  mock_resource "aws_lightsail_static_ip" {
    defaults = {
      ip_address = "198.51.100.20"
    }
  }
}

variables {
  name           = "me"
  ssh_public_key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleKeyExampleKeyExampleKeyExampleKey00 you@laptop"
  # tofu test destroys what it applied; the guard would refuse that.
  protect = false
}

# The walkthrough: own domain, home IPv4 only. IPv6 must be listed explicitly,
# with the placeholder, never left for AWS to fill in with ::/0.
run "domain_ipv4_only_ssh" {
  command = apply
  variables {
    public_domain = "me.example.com"
    ipv4          = true
    ssh_cidrs     = ["203.0.113.7/32"]
  }
  assert {
    condition = one([for p in aws_lightsail_instance_public_ports.server.port_info : p if p.from_port == 22]) == {
      protocol          = "tcp", from_port = 22, to_port = 22,
      cidrs             = toset(["203.0.113.7/32"]), ipv6_cidrs = toset(["2001:db8::/128"]),
      cidr_list_aliases = one([for p in aws_lightsail_instance_public_ports.server.port_info : p.cidr_list_aliases if p.from_port == 22])
    }
    error_message = "SSH must list 203.0.113.7/32 for IPv4 and only the placeholder for IPv6."
  }
  assert {
    condition = alltrue([for p in aws_lightsail_instance_public_ports.server.port_info :
      p.cidrs == toset(["0.0.0.0/0"]) && p.ipv6_cidrs == toset(["::/0"]) if contains([80, 443], p.from_port)
    ]) && length([for p in aws_lightsail_instance_public_ports.server.port_info : p if contains([80, 443], p.from_port)]) == 2
    error_message = "80 and 443 must be open to both families with a public domain."
  }
}

# The reverse case: a dual-stack server with only an IPv6 home prefix.
run "domain_ipv6_only_ssh" {
  command = apply
  variables {
    public_domain = "me.example.com"
    ipv4          = true
    ssh_cidrs     = ["2001:db8:1234::/48"]
  }
  assert {
    condition = alltrue([for p in aws_lightsail_instance_public_ports.server.port_info :
      p.cidrs == toset(["192.0.2.0/32"]) && p.ipv6_cidrs == toset(["2001:db8:1234::/48"]) if p.from_port == 22
    ])
    error_message = "SSH must list only the placeholder for IPv4 and the home prefix for IPv6."
  }
  assert {
    condition     = strcontains(output.next_steps, "| ssh admin@2001:db8:5::10 tar -x") && !strcontains(output.next_steps, "[")
    error_message = "With only IPv6 in ssh_cidrs, next_steps must reach the server over IPv6, as a bare address ssh can parse."
  }
}

# Both families listed: no placeholder.
run "domain_dual_ssh" {
  command = apply
  variables {
    public_domain = "me.example.com"
    ipv4          = true
    ssh_cidrs     = ["203.0.113.7/32", "2001:db8:1234::/48"]
  }
  assert {
    condition = alltrue([for p in aws_lightsail_instance_public_ports.server.port_info :
      p.cidrs == toset(["203.0.113.7/32"]) && p.ipv6_cidrs == toset(["2001:db8:1234::/48"]) if p.from_port == 22
    ])
    error_message = "SSH must list exactly ssh_cidrs."
  }
}

# Tailscale on an IPv6-only plan with an IPv6 SSH prefix. No IPv4 list is sent
# (the plan has no IPv4 firewall), and if AWS reports its default 0.0.0.0/0
# for that firewall, the postcondition must not fail.
run "tailscale_ipv6_plan_ssh" {
  command = apply
  variables {
    tailscale          = true
    tailscale_auth_key = "tskey-auth-test"
    ssh_cidrs          = ["2001:db8:1234::/48"]
  }
  override_resource {
    target = aws_lightsail_instance_public_ports.server
    # Applied to every rule: AWS reporting 0.0.0.0/0 for the missing IPv4 firewall.
    values = {
      port_info = { cidrs = ["0.0.0.0/0"] }
    }
  }
  assert {
    condition = alltrue([for p in aws_lightsail_instance_public_ports.server.port_info :
      p.cidrs == toset(["0.0.0.0/0"]) && p.ipv6_cidrs == toset(["2001:db8:1234::/48"]) if p.from_port == 22
    ])
    error_message = "The run must see AWS's IPv4 default on SSH, and still pass the postcondition."
  }
}

run "ipv6_plan_refuses_ipv4_ssh" {
  command = plan
  variables {
    tailscale          = true
    tailscale_auth_key = "tskey-auth-test"
    ssh_cidrs          = ["203.0.113.7/32"]
  }
  expect_failures = [aws_lightsail_instance.server]
}

# With Tailscale and ssh_cidrs, the bootstrap line must open OpenSSH on the
# host too, or the provider rule leads nowhere.
run "tailscale_with_ssh_cidrs_bootstrap" {
  command = apply
  variables {
    tailscale          = true
    tailscale_auth_key = "tskey-auth-test"
    ssh_cidrs          = ["2001:db8:1234::/48"]
  }
  assert {
    condition     = strcontains(output.next_steps, "--tag tag:me --ssh-from any'")
    error_message = "next_steps must bootstrap with --ssh-from any when ssh_cidrs is set."
  }
  assert {
    condition     = strcontains(output.next_steps, "git archive HEAD deploy/host deploy/systemd | ssh admin@me tar -x")
    error_message = "next_steps must ship only deploy/host and deploy/systemd."
  }
}

run "tailscale_only_bootstrap" {
  command = apply
  variables {
    tailscale          = true
    tailscale_auth_key = "tskey-auth-test"
  }
  assert {
    condition     = !strcontains(output.next_steps, "--ssh-from") && length([for p in aws_lightsail_instance_public_ports.server.port_info : p if p.from_port == 22]) == 0
    error_message = "Without ssh_cidrs there is no SSH rule and no --ssh-from."
  }
}
