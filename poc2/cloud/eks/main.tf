# EKS cluster backend for poc2 — an alternative to the local libvirt/kubeadm
# lab (see ../../infra/libvirt/main.tf) for running the di-agent-system Helm chart and the
# di-agent DaemonSet on a managed AWS Kubernetes cluster. Uses the widely
# used terraform-aws-modules building blocks instead of hand-rolled VPC/EKS
# resources to keep this module small and reviewable.

data "aws_availability_zones" "available" {
  state = "available"
}

module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "~> 6.7.2"

  name = "${var.cluster_name}-vpc"
  cidr = var.vpc_cidr

  azs             = slice(data.aws_availability_zones.available.names, 0, 3)
  private_subnets = [for i in range(3) : cidrsubnet(var.vpc_cidr, 4, i)]
  public_subnets  = [for i in range(3) : cidrsubnet(var.vpc_cidr, 4, i + 3)]

  enable_nat_gateway   = true
  single_nat_gateway   = true
  enable_dns_hostnames = true

  public_subnet_tags = {
    "kubernetes.io/role/elb" = "1"
  }
  private_subnet_tags = {
    "kubernetes.io/role/internal-elb" = "1"
  }
}

module "eks" {
  source  = "terraform-aws-modules/eks/aws"
  version = "~> 21.25.0"

  name               = var.cluster_name
  kubernetes_version = var.kubernetes_version

  vpc_id     = module.vpc.vpc_id
  subnet_ids = module.vpc.private_subnets

  endpoint_public_access       = true
  endpoint_private_access      = true
  endpoint_public_access_cidrs = var.api_access_cidrs

  # Let our own IAM caller manage the cluster via kubectl (module v21 no
  # longer grants this by default), which is needed to debug NodeCreationFailure.
  enable_cluster_creator_admin_permissions = true
  enable_irsa                              = true

  # vpc-cni must be up before nodes try to join, otherwise the managed node
  # group can report "NodeCreationFailure: Unhealthy nodes" because kubelet
  # never reaches Ready without pod networking. before_compute=true forces
  # the addon to be reconciled ahead of (and independent of) the node group,
  # avoiding the race that happens when everything is created in one apply.
  addons = {
    vpc-cni = {
      before_compute = true
    }
    kube-proxy = {}
    coredns    = {}
    aws-ebs-csi-driver = {
      most_recent              = true
      service_account_role_arn = aws_iam_role.ebs_csi.arn
    }
  }

  eks_managed_node_groups = {
    default = {
      min_size                   = var.node_count
      max_size                   = var.node_count + 2
      desired_size               = var.node_count
      instance_types             = [var.instance_type]
      ami_type                   = "AL2023_x86_64_STANDARD"
      create_access_entry        = true
      iam_role_attach_cni_policy = true
    }
  }
}

resource "aws_iam_role" "ebs_csi" {
  name = "${var.cluster_name}-ebs-csi"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect    = "Allow"
        Action    = "sts:AssumeRoleWithWebIdentity"
        Principal = { Federated = module.eks.oidc_provider_arn }
        Condition = {
          StringEquals = {
            "${module.eks.oidc_provider}:aud" = "sts.amazonaws.com"
            "${module.eks.oidc_provider}:sub" = "system:serviceaccount:kube-system:ebs-csi-controller-sa"
          }
        }
      }
    ]
  })
}

resource "aws_iam_role_policy_attachment" "ebs_csi" {
  role       = aws_iam_role.ebs_csi.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonEBSCSIDriverPolicy"
}
