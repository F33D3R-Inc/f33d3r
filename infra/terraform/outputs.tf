# F33D3R Terraform Outputs

output "master_ip" {
  description = "k3s master node public IP"
  value       = hcloud_server.k3s_master.ipv4_address
}

output "worker_ips" {
  description = "k3s worker node public IPs"
  value       = hcloud_server.k3s_worker[*].ipv4_address
}

output "cluster_endpoint" {
  description = "k3s API server endpoint"
  value       = "https://${hcloud_server.k3s_master.ipv4_address}:6443"
}

output "app_url" {
  description = "F33D3R application URL"
  value       = var.environment == "production" ? "https://f33d3r.app" : "https://staging.f33d3r.app"
}

output "postgres_host" {
  description = "Managed Postgres host (production only)"
  value       = length(hcloud_managed_database.f33d3r_postgres) > 0 ? hcloud_managed_database.f33d3r_postgres[0].host : "self-hosted in k3s"
  sensitive   = true
}

output "ssh_command" {
  description = "SSH command to access master node"
  value       = "ssh root@${hcloud_server.k3s_master.ipv4_address}"
}
