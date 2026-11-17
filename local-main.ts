import { createApp } from './app.ts';
const server = createApp();
server.listen(Number(process.env.PORT ?? 8901), '127.0.0.1', () => {
  process.stdout.write(`listening ${process.env.PORT ?? 8901}\n`);
});
