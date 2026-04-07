import threading
import time
import pytest
from entroq.pg import DependencyError, StopWorker, TaskData

def test_worker_fail_fast_on_lease_loss(eq):
    """Verify that worker stops immediately when its task is stolen,
    due to TaskProxy health checks.
    """
    queue = '/test/fail-fast'
    eq.modify(inserts=[TaskData(queue=queue, value='work')])
    
    worker_stopped = threading.Event()
    
    def handle(renew, finalize):
        with renew as t:
            try:
                # Wait for the task to be "stolen".
                # Loop and access attributes to trigger health checks.
                for _ in range(50):
                    time.sleep(0.1)
                    _ = t.id # Accessing ID should trigger health check
            except DependencyError:
                worker_stopped.set()
                raise StopWorker
    
    # 2. Start worker
    from entroq.pg import EQWorker, EntroQ
    w = EQWorker(eq)
    # Short claim duration so renewer fires frequently (every 1s).
    t = threading.Thread(target=w.work, args=(queue, handle), 
                         kwargs={'claim_duration': 2}, daemon=True)
    t.start()
    
    time.sleep(0.5) # Let worker claim and enter its handle()
    
    # 3. Steal task using a DIFFERENT client (different claimant ID)
    # This simulates another worker stealing it or eviction.
    eq2 = EntroQ(eq._connstr)
    tasks = eq2.tasks(queue=queue)
    assert len(tasks) == 1, f"Task should be in queue, got {len(tasks)}"
    eq2.modify(deletes=[tasks[0]])
    
    # 4. Verify worker stops quickly (due to fail-fast health check)
    assert worker_stopped.wait(timeout=5), "Worker did not fail fast on lease loss"
    t.join(timeout=2)
