Example:

$ node
> const grpc = require('grpc');
> const pb = require('./entroq_pb');
> const pbgrpc = require('entroq_grpc_pb');
> const eqc = pbgrpc.EntroQClient("entroq.support:37706", grpc.credentials.createInsecure());
> var callStats = eqc.queueStats(new pb.QueuesRequest(), (err, resp) => console.log(resp));
