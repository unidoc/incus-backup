import { stopAll } from './backend'
export default async function globalTeardown() { stopAll() }
