// Where each contract is deployed: the one list the portal shows. Sources: the deployment records
// (CreateX deploy; the devnet program deployed final,
// with no upgrade authority), checked on chain. ForwarderImpl is the Factory's `implementation()`.
// A network without an address is not deployed yet: never put a predicted address here.

export type Contract = 'Factory' | 'ForwarderImpl' | 'Gateway' | 'Forwarder program';

export interface Deployment {
  network: string;
  address: string | null;
  explorer: string | null;
}

const basescan = (a: string): Deployment => ({ network: 'Base', address: a, explorer: `https://basescan.org/address/${a}` });
const basescanSepolia = (a: string): Deployment => ({ network: 'Base Sepolia', address: a, explorer: `https://sepolia.basescan.org/address/${a}` });
const solanaDevnet = (a: string): Deployment => ({ network: 'Solana devnet', address: a, explorer: `https://explorer.solana.com/address/${a}?cluster=devnet` });
const notYet = (network: string): Deployment => ({ network, address: null, explorer: null });

export const DEPLOYMENTS: Record<Contract, Deployment[]> = {
  Factory: [basescan('0x7575EcdC25535417f9BF0dBbdbc62247d8B5CAE3'), basescanSepolia('0xeDf18bea50831Be317989f96ADdA44a51206eb63')],
  ForwarderImpl: [basescan('0x16ADd0690c473570E2C42e6cB622F20301E0d49B'), basescanSepolia('0x599Ea6c830d446C104e32981db570Ba61B946208')],
  Gateway: [basescan('0x17C8cF69ca9D1d15dCE1546d00010B290f973143'), basescanSepolia('0x0b7602D318542F24f015a93930Fa31cf06d5E6A1')],
  'Forwarder program': [solanaDevnet('E4CUnAM7uUyfCrqRay8x7gRbZ4aDCE8DBoep8FgpsYp4'), notYet('Solana mainnet')],
};
