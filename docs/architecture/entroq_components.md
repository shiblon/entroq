# EntroQ Components

## Overview

EntroQ is a distributed task queue that provides a scalable and fault-tolerant way to manage tasks. The following components are used in the strict ordering of heterogeneous tasks use case:

* Queues: tenants, request_A, request_B, request_C, etc., for task types A, B, C, etc., and ephemeral response queues.
* Workers: tenant worker, A worker, B worker, C worker, etc., for reading from their respective request queues.

## Tenant Worker

The tenant worker is responsible for locking the tenant's tasks in the ordered storage. It does the following each time a task is successfully claimed from the "tenants" queue:

* Go to ordered storage for the tenant represented in the claimed task.
* Get the head item.
* Do what it says to do.
* If successful, delete the head item from the task list.
* Set the claimed task's Arrival Time to "now".

## Microservice Workers

In the microservice version, the tenant worker does not carry out the work for every possible task type. Instead, it sends requests for each type to be completed by another service. The microservice workers are responsible for reading from their respective request queues and completing the tasks.