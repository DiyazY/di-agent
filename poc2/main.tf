# Download Ubuntu 22.04 (Jammy) cloud image
# Ubuntu cloud images have excellent cloud-init support
resource "libvirt_volume" "ubuntu_base" {
  name   = "ubuntu-jammy-base.qcow2"
  pool   = "default"
  target = {
    format = {
      type = "qcow2"
    }
  }

  create = {
    content = {
      # Ubuntu 22.04 LTS (Jammy Jellyfish) cloud image
      url = var.ubuntu_22_img_url
    }
  }
}

# Create boot disk for VM1 (uses base image as backing store)
resource "libvirt_volume" "vm_disk" {
  count  = var.vm_count
  name   = "vm${count.index + 1}-disk.index"
  pool   = "default"
  target = {
    format = {
      type = "qcow2"
    }
  }

  capacity      = 20 
  capacity_unit = "GiB"

  backing_store = {
    path   = libvirt_volume.ubuntu_base.path
    format = {
      type = "qcow2"
    }
  }
}

# Cloud-init configuration for VM1
resource "libvirt_cloudinit_disk" "vm_init" {
  name = "vm-cloudinit"

  # User-data: Configure root password, enable SSH, install packages
  user_data      = file("${path.module}/config/cloud_init.yml")
  # Meta-data: Instance identification
  meta_data      = file("${path.module}/config/metadata.yml") 

}

# Upload cloud-init ISO for VM1 to a volume
resource "libvirt_volume" "vm_cloudinit" {
  count  = var.vm_count
  name = "vm${count.index + 1}-cloudinit.iso"
  pool = "default"
  # Format will be auto-detected as "iso"

  create = {
    content = {
      url = libvirt_cloudinit_disk.vm_init.path
    }
  }
}

# Virtual Machine 1
resource "libvirt_domain" "vm1" {
  count  = var.vm_count
  name   = "${var.vm_hostname}${count.index + 1}"
  memory = 3 # 3 GB
  memory_unit = "GiB"
  vcpu   = 1
  type   = "kvm"

  # Boot configuration
  os = {
    type    = "hvm"
    type_arch    = "x86_64"
    type_machine = "q35"
  }

  # Attached disks
  devices = {
    disks = [
      # Main system disk
      {
        source = {
          volume = {
            pool   = libvirt_volume.vm_disk[count.index].pool
            volume = libvirt_volume.vm_disk[count.index].name
          }
        }
        target = {
          bus = "virtio"
          dev = "vda"
        }
        driver = {
          type = "qcow2"
        }
        boot = {
          order = 1
        }
      },
      # Cloud-init config disk (will be detected automatically)
      {
        device = "cdrom"
        source = {
          volume = {
            pool   = libvirt_volume.vm_cloudinit[count.index].pool
            volume = libvirt_volume.vm_cloudinit[count.index].name
          }
        }
        target = {
          bus = "sata"
          dev = "sdb"
        }
        boot = {
          order = 2
        }
      }
    ]

    # Network interface on default network (DHCP)
    interfaces = [
      # Network interface connected to default network
      {
        model = { 
          type = "virtio" 
        }
        source = {
          network = {
            network = "default"
          }
        }

        // This is for bridge network
        # source = {
        #   bridge = {
        #     bridge = "nm-bridge"
        #   }
        # }
      }
    ]

    # Graphics console (VNC)
    graphics = [
      {
        vnc = {
          auto_port = true
          listen    = "127.0.0.1"
        }
      }
    ]

    # Serial console for virsh console access
    consoles = [
      {
        target = {
          type = "serial"
          port = 0
        }
      }
    ]
  }

  # Start the VM automatically
  running = true
}

output "instructions" {
  value = <<-EOF

    Virtual machines have been created!

    To find IP addresses assigned by DHCP:
      sudo virsh domifaddr ubuntu-vm1
      sudo virsh domifaddr ubuntu-vm2
      sudo virsh domifaddr ubuntu-vm3

    Or check the DHCP leases:
      sudo virsh net-dhcp-leases default

    To connect via SSH (once you know the IP):
      ssh root@<IP-ADDRESS>
      Password: password

    To view VM console:
      sudo virsh console ubuntu-vm1
      sudo virsh console ubuntu-vm2
      sudo virsh console ubuntu-vm3

    To connect via VNC:
      1. Find VNC port: sudo virsh domdisplay ubuntu-vm1
      2. Connect with VNC client to that address

    Note: It may take 30-60 seconds after boot for cloud-init to complete
          and the SSH server to be available.
  EOF
}