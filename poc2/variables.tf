variable "ubuntu_22_img_url" {
  description = "ubuntu 22.04 image"
  default     = "https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-amd64.img"
}

variable "vm_hostname" {
  description = "vm hostname"
  default     = "ubuntu-vm"
}

variable "vm_count" {
  description = "Number of VMs to create"
  default     = 1
}

variable "pool_name" {
  description = "Name of the pool"
  default     = "pool_1"
}

variable "pool_path" {
  description = "Path of the pool"
  default     = "/var/lib/libvirt/images/pool_1"
}