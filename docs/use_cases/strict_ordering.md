# Strict Ordering of Heterogeneous Tasks

## Overview

This use case describes how to achieve strict ordering of heterogeneous tasks using EntroQ. This is particularly useful when multiple tenants have strong requirements that requests be handled in precisely the order submitted.

## Storage Types Needed

Two storage mechanisms are required:

* Ordered storage: This can be any storage mechanism that supports appending to the tail, getting the head, and deleting the head. Examples include a Postgres table with tasks sorted by monotonically increasing object ID or timestamp.
* EntroQ: This is used as a lock per tenant to ensure strict ordering of task completion.

## Intake

To get tasks created per tenant, the ordered storage mechanism is used to communicate between the UI and backend. The UI creates tasks and pushes them into ordered storage, which can be a database or a flat file with an outside locking mechanism.

## Single Worker Solution

This solution involves setting up a single queue in EntroQ called "tenants". When a new tenant comes online, a single task for that tenant is added to this queue. This task lives forever and is used to lock the tenant's tasks in the ordered storage.

A "tenant worker" type is created that does the following each time a task is successfully claimed from the "tenants" queue:

* Go to ordered storage for the tenant represented in the claimed task.
* Get the head item.
* Do what it says to do.
* If successful, delete the head item from the task list.
* Set the claimed task's Arrival Time to "now".

## Microservice Version

In this version, the tenant worker does not carry out the work for every possible task type. Instead, it sends requests for each type to be completed by another service. If EntroQ is used for async comms, the recipe has additional components:

* Queues: tenants, request_A, request_B, request_C, etc., for task types A, B, C, etc., and ephemeral response queues.
* Workers: tenant worker, A worker, B worker, C worker, etc., for reading from their respective request queues.

The tenant worker flow is as follows:

* Go to ordered storage, get head task for currently claimed tenant.
* Create a response queue name, e.g., "tenant1_response_25829234832234".
* Create an EntroQ task with metadata indicating how to complete it, and package the response queue name within it.
* Claim respQ - this blocks indefinitely.
* If successful, delete head item in ordered storage for this tenant.
* Set AT on tenant task to now.

## Notes on Idempotence

When writing to a database within a worker, it is best practice to have the task describe "how the database should look after finishing" instead of "how to incrementally change whatever is there". Describe end state, not diffs.

When writing files, you want to do the following:

* Always write to a timestamped file name.
* Always write to a "partial" file name.
* Only when finished should the file name look real.
* Use a separate process to garbage collect old partial files.

Example:

* Open "my-output-file-20230630-110800.partial" for writing.
* Start writing.
* Finish writing.
* Rename to "my-output-file-20230630-110800".

By just doing a file listing, you can see the state of every file, you can tell which ones are old and corrupt, you can see which ones are finished, and you will *never have two workers writing bytes to the same file at the same time*.

Always write unique filenames, and then indicate them in the response instead of just assuming they'll be called something standard. It's the only way to really be safe.