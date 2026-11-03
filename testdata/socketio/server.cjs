// Official Socket.IO server, mirroring ABS's auth/init flow and event names.
const http = require('node:http');
const { Server } = require('socket.io');
const server = http.createServer();
const io = new Server(server, {
  path: '/audiobookshelf/socket.io',
  pingInterval: 50,
  pingTimeout: 1000,
});
io.on('connection', socket => {
  socket.on('auth', token => {
    if (token !== 'reference-token') {
      socket.emit('auth_failed', { message: 'Invalid token' });
      return;
    }
    socket.emit('init', { userId: 'test-user' });
    // Several heartbeat exchanges must succeed before the update is delivered.
    setTimeout(() => socket.emit('playlist_updated', { id: 'playlist-a' }), 300);
  });
});
server.listen(0, '127.0.0.1', () => {
  console.log(`http://127.0.0.1:${server.address().port}/audiobookshelf`);
});
