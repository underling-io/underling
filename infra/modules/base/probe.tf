# One-off Fargate task that runs the image's sandbox probe (016.02) under the
# service's security settings: same image, user, writable root and init
# process, no extra capabilities. It needs no task role and no secrets.
resource "aws_ecs_task_definition" "probe" {
  family                   = "${var.name}-probe"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = 512
  memory                   = 1024
  execution_role_arn       = aws_iam_role.execution.arn

  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "ARM64"
  }

  container_definitions = jsonencode([
    {
      name      = var.name
      image     = "${aws_ecr_repository.this.repository_url}:${var.image_tag}"
      essential = true
      command   = ["underling-sandbox-probe"]

      linuxParameters = {
        initProcessEnabled = true
      }

      logConfiguration = {
        logDriver = "awslogs"
        options = {
          awslogs-group         = aws_cloudwatch_log_group.this.name
          awslogs-region        = var.aws_region
          awslogs-stream-prefix = "probe"
        }
      }
    }
  ])
}
