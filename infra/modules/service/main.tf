resource "aws_ecs_task_definition" "this" {
  family                   = var.name
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.task_cpu
  memory                   = var.task_memory
  execution_role_arn       = var.execution_role_arn
  task_role_arn            = var.task_role_arn

  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "ARM64"
  }

  ephemeral_storage {
    size_in_gib = var.ephemeral_storage_gib
  }

  # The image's CMD runs heinzel on :8080. No container healthCheck: the
  # ALB's /healthz check is the one that counts.
  container_definitions = jsonencode([
    {
      name      = var.name
      image     = var.image
      essential = true

      portMappings = [
        {
          containerPort = 8080
          protocol      = "tcp"
        }
      ]

      environment = [
        for name in sort(keys(var.environment)) : { name = name, value = var.environment[name] }
      ]
      secrets = [
        for name in sort(keys(var.secrets)) : { name = name, valueFrom = var.secrets[name] }
      ]

      stopTimeout = 30

      # An init process reaps the orphans the claude child leaves behind.
      linuxParameters = {
        initProcessEnabled = true
      }

      logConfiguration = {
        logDriver = "awslogs"
        options = {
          awslogs-group         = var.log_group_name
          awslogs-region        = var.aws_region
          awslogs-stream-prefix = var.name
        }
      }
    }
  ])
}

resource "aws_ecs_service" "this" {
  name             = var.name
  cluster          = var.cluster_arn
  task_definition  = aws_ecs_task_definition.this.arn
  launch_type      = "FARGATE"
  platform_version = "LATEST"
  desired_count    = 1

  # Stop the old task before starting the new one, so two writers never
  # share the event store; a deploy has a short window with no receiver.
  deployment_minimum_healthy_percent = 0
  deployment_maximum_percent         = 100

  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

  availability_zone_rebalancing     = "DISABLED"
  health_check_grace_period_seconds = 60

  network_configuration {
    subnets          = var.public_subnet_ids
    security_groups  = [var.task_security_group_id]
    assign_public_ip = true
  }

  load_balancer {
    target_group_arn = aws_lb_target_group.this.arn
    container_name   = var.name
    container_port   = 8080
  }

  wait_for_steady_state = true

  depends_on = [
    aws_lb_listener.https,
    aws_lb_listener_rule.webhook,
    aws_vpc_security_group_ingress_rule.task_from_alb,
  ]
}
