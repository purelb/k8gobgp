Before doing anything else, perform a cluster discovery and write the results to .claude-cluster-state.md:
1. Run `kubectl config current-context` and `kubectl config get-contexts`
2. List all nodes with labels and taints: `kubectl get nodes -o wide --show-labels`
3. List all PureLB-related resources: services with type LoadBalancer, ServiceGroup CRs, lbnodeagent pods
4. Check PureLB controller logs for recent errors
5. Write all findings to the state file with timestamps

For every kubectl command you run in this session, first verify the context matches what's in the state file. If anything changes (new deploys, deletes), update the state file.

Now, with this context established: [YOUR ACTUAL REQUEST]