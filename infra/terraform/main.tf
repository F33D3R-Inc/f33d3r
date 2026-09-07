# F33D3R — Terraform Infrastructure
# Provisions VPS nodes for k3s cluster.
# Provider: Hetzner Cloud (cost-effective, EU-compliant)
# Change provider block for AWS/GCP/DO as needed.

terraform {
  required_version = ">= 1.7.0"

  required_providers {
    hcloud = {
      source  = "hetznercloud/hcloud"
      version = "~> 1.47"
    }
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "~> 4.0"
    }
  }

  backend "http" {
    # GitLab-managed Terraform state
    # Set TF_HTTP_ADDRESS, TF_HTTP_LOCK_ADDRESS etc. in CI variables
  }
}

# ── Variables ─────────────────────────────────────────────────────────────────

variable "hcloud_token" {
  description = "Hetzner Cloud API token"
  type        = string
  sensitive   = true
}

variable "cloudflare_api_token" {
  description = "Cloudflare API token"
  type        = string
  sensitive   = true
}

variable "cloudflare_zone_id" {
  description = "Cloudflare zone ID for f33d3r.app"
  type        = string
}

variable "commit_sha" {
  description = "Git commit SHA for image tags"
  type        = string
  default     = "latest"
}

variable "environment" {
  description = "Deployment environment: production | staging"
  type        = string
  default     = "production"
}

variable "ssh_public_key" {
  description = "SSH public key for server access"
  type        = string
}

# ── Providers ─────────────────────────────────────────────────────────────────

provider "hcloud" {
  token = var.hcloud_token
}

provider "cloudflare" {
  api_token = var.cloudflare_api_token
}

# ── SSH Key ───────────────────────────────────────────────────────────────────

resource "hcloud_ssh_key" "f33d3r_deploy" {
  name       = "f33d3r-deploy-${var.environment}"
  public_key = var.ssh_public_key
}

# ── Network ───────────────────────────────────────────────────────────────────

resource "hcloud_network" "f33d3r_net" {
  name     = "f33d3r-${var.environment}"
  ip_range = "10.0.0.0/16"
}

resource "hcloud_network_subnet" "f33d3r_subnet" {
  network_id   = hcloud_network.f33d3r_net.id
  type         = "cloud"
  network_zone = "eu-central"
  ip_range     = "10.0.1.0/24"
}

# ── Firewall ──────────────────────────────────────────────────────────────────

resource "hcloud_firewall" "f33d3r_fw" {
  name = "f33d3r-${var.environment}-fw"

  rule {
    direction  = "in"
    protocol   = "tcp"
    port       = "22"
    source_ips = ["0.0.0.0/0", "::/0"]
    description = "SSH"
  }
  rule {
    direction  = "in"
    protocol   = "tcp"
    port       = "80"
    source_ips = ["0.0.0.0/0", "::/0"]
    description = "HTTP"
  }
  rule {
    direction  = "in"
    protocol   = "tcp"
    port       = "443"
    source_ips = ["0.0.0.0/0", "::/0"]
    description = "HTTPS"
  }
  rule {
    direction  = "in"
    protocol   = "tcp"
    port       = "6443"
    source_ips = ["10.0.0.0/8"]
    description = "k3s API (internal)"
  }
}

# ── Compute — k3s master ──────────────────────────────────────────────────────

resource "hcloud_server" "k3s_master" {
  name        = "f33d3r-${var.environment}-master"
  image       = "ubuntu-22.04"
  server_type = var.environment == "production" ? "cpx31" : "cpx21"
  location    = "nbg1"
  ssh_keys    = [hcloud_ssh_key.f33d3r_deploy.id]
  firewall_ids = [hcloud_firewall.f33d3r_fw.id]

  network {
    network_id = hcloud_network.f33d3r_net.id
    ip         = "10.0.1.10"
  }

  user_data = templatefile("${path.module}/userdata/master.sh", {
    environment  = var.environment
    cluster_name = "f33d3r-${var.environment}"
  })

  labels = {
    role        = "master"
    environment = var.environment
    managed-by  = "terraform"
  }
}

# ── Compute — k3s worker nodes ────────────────────────────────────────────────

resource "hcloud_server" "k3s_worker" {
  count       = var.environment == "production" ? 2 : 1
  name        = "f33d3r-${var.environment}-worker-${count.index}"
  image       = "ubuntu-22.04"
  server_type = var.environment == "production" ? "cpx31" : "cpx21"
  location    = "nbg1"
  ssh_keys    = [hcloud_ssh_key.f33d3r_deploy.id]
  firewall_ids = [hcloud_firewall.f33d3r_fw.id]

  network {
    network_id = hcloud_network.f33d3r_net.id
    ip         = "10.0.1.${11 + count.index}"
  }

  labels = {
    role        = "worker"
    environment = var.environment
    managed-by  = "terraform"
  }

  depends_on = [hcloud_server.k3s_master]
}

# ── Managed Database (Postgres) ───────────────────────────────────────────────

resource "hcloud_managed_database" "f33d3r_postgres" {
  count      = var.environment == "production" ? 1 : 0
  name       = "f33d3r-${var.environment}-pg"
  type       = "pg"
  plan       = "business-16"
  location   = "nbg1"
  network_id = hcloud_network.f33d3r_net.id
}

# ── DNS (Cloudflare) ──────────────────────────────────────────────────────────

resource "cloudflare_record" "f33d3r_root" {
  zone_id = var.cloudflare_zone_id
  name    = var.environment == "production" ? "@" : "staging"
  type    = "A"
  value   = hcloud_server.k3s_master.ipv4_address
  proxied = true
  ttl     = 1
}

resource "cloudflare_record" "f33d3r_www" {
  count   = var.environment == "production" ? 1 : 0
  zone_id = var.cloudflare_zone_id
  name    = "www"
  type    = "A"
  value   = hcloud_server.k3s_master.ipv4_address
  proxied = true
  ttl     = 1
}
