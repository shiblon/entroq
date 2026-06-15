# Idempotence

## Overview

Idempotence is the property of an operation that can be applied multiple times without changing the result beyond the initial application. In the context of task queues, idempotence is crucial to ensure that tasks can be safely retried without causing unintended side effects.

## Writing to a Database

When writing to a database within a worker, it is best practice to have the task describe "how the database should look after finishing" instead of "how to incrementally change whatever is there". Describe end state, not diffs.

## Writing Files

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