import AllocationRow from 'nomad-ui/components/allocation-row';

export default AllocationRow.extend({
  pluginAllocation: null,
  allocation: null,

  didReceiveAttrs() {
    this.setAllocation();
  },

  // The allocation for the plugin's controller or storage plugin needs
  // to be imperatively fetched since these plugins are Fragments which
  // can't have relationships.
  async setAllocation() {
    if (this.pluginAllocation && !this.allocation) {
      const allocation = await this.pluginAllocation.getAllocation();
      if (!this.isDestroyed) {
        this.set('allocation', allocation);
        this.updateStatsTracker();
      }
    }
  },
});
