# One VPC, two public subnets, no NAT: the ALB and the task both sit in the
# public subnets and the task gets a public IP for egress.

data "aws_availability_zones" "available" {
  state = "available"
}

resource "aws_vpc" "this" {
  cidr_block           = "10.42.0.0/16"
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = {
    Name = var.name
  }
}

resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id

  tags = {
    Name = var.name
  }
}

resource "aws_subnet" "public" {
  count = 2

  vpc_id            = aws_vpc.this.id
  availability_zone = data.aws_availability_zones.available.names[count.index]
  cidr_block        = cidrsubnet(aws_vpc.this.cidr_block, 8, count.index)

  tags = {
    Name = "${var.name}-public-${data.aws_availability_zones.available.names[count.index]}"
  }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.this.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.this.id
  }

  tags = {
    Name = "${var.name}-public"
  }
}

resource "aws_route_table_association" "public" {
  count = 2

  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

# The task's only ingress, 8080 from the ALB, is added in module.service.
resource "aws_security_group" "task" {
  name        = "${var.name}-task"
  description = "underling tasks"
  vpc_id      = aws_vpc.this.id

  tags = {
    Name = "${var.name}-task"
  }
}

resource "aws_vpc_security_group_egress_rule" "task_all" {
  security_group_id = aws_security_group.task.id
  description       = "All egress: Bedrock, GitHub, ECR, Secrets Manager, CloudWatch Logs"
  ip_protocol       = "-1"
  cidr_ipv4         = "0.0.0.0/0"
}
